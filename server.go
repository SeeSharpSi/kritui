package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/tools"

	_ "modernc.org/sqlite"
)

//go:embed static
var staticFiles embed.FS

//go:embed db/schema.sql
var schema string

const (
	serverReadHeaderTimeout = 5 * time.Second
	serverIdleTimeout       = 2 * time.Minute
	databaseBusyTimeout     = 5 * time.Second
)

func main() {
	databaseExists := true
	if _, err := os.Stat("data.db"); errors.Is(err, os.ErrNotExist) {
		databaseExists = false
	} else if err != nil {
		log.Fatalf("check database: %v", err)
	}

	database, err := openDatabase("data.db")
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	if !databaseExists {
		if _, err := database.Exec(schema); err != nil {
			log.Fatalf("initialize database: %v", err)
		}
	} else if err := migrateDatabase(database); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	if err := kritui_db.EnsureDefaultModel(context.Background(), database, os.Getenv("LLM_MODEL")); err != nil {
		log.Fatalf("initialize settings: %v", err)
	}
	toolRegistry, err := tools.NewRegistry(
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(os.Getenv("SEARXNG_URL")),
		tools.NewGitTool(),
	)
	if err != nil {
		log.Fatalf("register tools: %v", err)
	}
	var toolCallLogger *log.Logger
	switch value := os.Getenv("SHOW_TOOLCALLS"); value {
	case "", "false":
	case "true":
		toolCallLogger = log.Default()
	default:
		log.Fatalf("SHOW_TOOLCALLS must be true or false, got %q", value)
	}

	mux := http.NewServeMux()
	toolCalls := newToolCallStore()
	mux.HandleFunc("GET /healthz", healthHandler(database))
	mux.HandleFunc("GET /{$}", homeHandler(database, toolRegistry))
	mux.HandleFunc("GET /history", historyHandler(database))
	mux.HandleFunc("GET /settings", settingsHandler(database, toolRegistry))
	mux.HandleFunc("POST /settings", settingsHandler(database, toolRegistry))
	mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	mux.HandleFunc("POST /messages", messageHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/retry", messageRetryHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/complete", messageCompletionHandler(database, toolRegistry, toolCalls, toolCallLogger))
	mux.HandleFunc("GET /messages/tools", messageToolStreamHandler(toolCalls))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFiles)))
	mux.HandleFunc("GET /sw.js", serviceWorkerHandler(staticFiles))

	log.Println("listening on http://localhost:8080")
	server := newHTTPServer(mux)
	// SSE responses intentionally have no server-wide write deadline.
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func openDatabase(path string) (*sql.DB, error) {
	parameters := url.Values{}
	parameters.Add("_pragma", "foreign_keys(1)")
	parameters.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", databaseBusyTimeout.Milliseconds()))
	parameters.Add("_pragma", "journal_mode(DELETE)")
	dataSourceName := (&url.URL{Scheme: "file", Opaque: path, RawQuery: parameters.Encode()}).String()
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, err
	}
	// One rollback-journal connection serializes in-process writes.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return database, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

func serviceWorkerHandler(files embed.FS) http.HandlerFunc {
	worker, err := files.ReadFile("static/sw.js")
	if err != nil {
		panic("embedded service worker missing")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Service-Worker-Allowed", "/")
		_, _ = w.Write(worker)
	}
}

func healthHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var value int
		if err := database.QueryRowContext(r.Context(), `SELECT 1`).Scan(&value); err != nil || value != 1 {
			if err != nil {
				log.Printf("health check database: %v", err)
			} else {
				log.Printf("health check database returned %d", value)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, "unhealthy")
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	}
}

func migrateDatabase(database *sql.DB) error {
	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(databaseMigrations) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, len(databaseMigrations))
	}

	for version < len(databaseMigrations) {
		nextVersion := version + 1
		if err := databaseMigrations[version](ctx, tx); err != nil {
			return fmt.Errorf("migrate database to version %d: %w", nextVersion, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, nextVersion)); err != nil {
			return fmt.Errorf("record schema version %d: %w", nextVersion, err)
		}
		version = nextVersion
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

var databaseMigrations = []func(context.Context, *sql.Tx) error{
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "messages", "model", `TEXT`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "chats", "tools", `TEXT NOT NULL DEFAULT '[]' CHECK (
			CASE
				WHEN json_valid(tools) THEN json_type(tools) = 'array'
				ELSE 0
			END
		)`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		if err := addColumnIfMissing(ctx, tx, "messages", "total_tokens", `INTEGER`); err != nil {
			return err
		}
		return addColumnIfMissing(ctx, tx, "messages", "cost", `REAL`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settings (
			name TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) STRICT`)
		return err
	},
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "messages", "provider_metadata", `TEXT CHECK (
			provider_metadata IS NULL OR
			CASE
				WHEN role = 'assistant' AND json_valid(provider_metadata) THEN json_type(provider_metadata) = 'object'
				ELSE 0
			END
		)`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "chats", "appends", `TEXT NOT NULL DEFAULT '[]' CHECK (
			CASE
				WHEN json_valid(appends) THEN json_type(appends) = 'array'
				ELSE 0
			END
		)`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "messages", "prompt_appends", `TEXT CHECK (
			prompt_appends IS NULL OR
			CASE
				WHEN json_valid(prompt_appends) THEN json_type(prompt_appends) = 'array'
				ELSE 0
			END
		) CHECK (prompt_appends IS NULL OR role = 'user')`)
	},
}

func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}

	tableExists := false
	columnExists := false
	for rows.Next() {
		tableExists = true
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		if name == column {
			columnExists = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if !tableExists {
		return fmt.Errorf("required table %s does not exist", table)
	}
	if columnExists {
		return nil
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %q ADD COLUMN %q %s`, table, column, definition)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}
