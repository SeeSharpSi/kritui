package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"seesharpsi/kritui/commands"
	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/tools"
	"seesharpsi/kritui/tools/git"

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
	toolList := []tools.Tool{
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(os.Getenv("SEARXNG_URL")),
	}
	toolList = append(toolList, git.NewGitTools()...)
	toolRegistry, err := tools.NewRegistry(toolList...)
	if err != nil {
		log.Fatalf("register tools: %v", err)
	}
	commandRegistry, err := newCommandRegistry(database)
	if err != nil {
		log.Fatalf("register commands: %v", err)
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
	mux.HandleFunc("GET /{$}", homeHandler(database, toolRegistry, commandRegistry, toolCalls))
	mux.HandleFunc("GET /history", historyHandler(database))
	mux.HandleFunc("GET /settings", settingsHandler(database, toolRegistry))
	mux.HandleFunc("POST /settings", settingsHandler(database, toolRegistry))
	mux.HandleFunc("POST /settings/ntfy", ntfySettingsHandler(database))
	mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	mux.HandleFunc("POST /messages", messageHandler(database, toolRegistry, commandRegistry, toolCalls))
	mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/retry", messageRetryHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/complete", messageCompletionHandler(database, toolRegistry, toolCalls, toolCallLogger))
	mux.HandleFunc("GET /messages/tools", messageToolStreamHandler(toolCalls))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFiles)))
	mux.HandleFunc("GET /favicon.ico", faviconHandler(staticFiles))
	mux.HandleFunc("GET /sw.js", serviceWorkerHandler(staticFiles))

	log.Println("listening on http://localhost:8080")
	server := newHTTPServer(mux)
	// SSE responses intentionally have no server-wide write deadline.
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func newCommandRegistry(database *sql.DB) (*commands.Registry, error) {
	newCommand, err := commands.NewRedirectCommand("new", "Start a new chat", "/")
	if err != nil {
		return nil, err
	}
	undo, err := commands.NewMessageHistoryCommand("undo", "Undo the last message", func(ctx context.Context, chatID int64) (templ.Component, error) {
		result, err := kritui_db.UndoLatestTurn(ctx, database, chatID)
		switch {
		case errors.Is(err, kritui_db.ErrNothingToUndo):
			return nil, &commands.UserError{Status: http.StatusConflict, Message: "Nothing to undo."}
		case errors.Is(err, kritui_db.ErrChatNotFound):
			return nil, &commands.UserError{Status: http.StatusNotFound, Message: "Chat not found."}
		case err != nil:
			return nil, fmt.Errorf("undo current chat: %w", err)
		}
		return templates.MessageHistoryResult(strconv.FormatInt(chatID, 10), result.Messages, result.Message.Content), nil
	})
	if err != nil {
		return nil, err
	}
	redo, err := commands.NewMessageHistoryCommand("redo", "Redo the last undone message", func(ctx context.Context, chatID int64) (templ.Component, error) {
		messages, err := kritui_db.RedoLatestTurn(ctx, database, chatID)
		switch {
		case errors.Is(err, kritui_db.ErrNothingToRedo):
			return nil, &commands.UserError{Status: http.StatusConflict, Message: "Nothing to redo."}
		case errors.Is(err, kritui_db.ErrChatNotFound):
			return nil, &commands.UserError{Status: http.StatusNotFound, Message: "Chat not found."}
		case err != nil:
			return nil, fmt.Errorf("redo current chat: %w", err)
		}
		return templates.MessageHistoryResult(strconv.FormatInt(chatID, 10), messages, ""), nil
	})
	if err != nil {
		return nil, err
	}
	history, err := commands.NewPanelCommand("history", "Open chat history", "history-page")
	if err != nil {
		return nil, err
	}
	settings, err := commands.NewPanelCommand("settings", "Open settings", "settings-page")
	if err != nil {
		return nil, err
	}
	rename, err := commands.NewRenameCommand(func(ctx context.Context, chatID int64, title string) error {
		title = normalizeChatTitle(title)
		if title == "" {
			return &commands.UserError{Status: http.StatusBadRequest, Message: "Usage: /rename <title>."}
		}
		result, err := database.ExecContext(ctx, `
			UPDATE chats
			SET title = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?
		`, title, chatID)
		if err != nil {
			return fmt.Errorf("rename current chat: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("get renamed chat count: %w", err)
		}
		if affected == 0 {
			return &commands.UserError{Status: http.StatusNotFound, Message: "Chat not found."}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return commands.NewRegistry(newCommand, undo, redo, history, settings, rename)
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

func faviconHandler(files embed.FS) http.HandlerFunc {
	icon, err := files.ReadFile("static/favicon.ico")
	if err != nil {
		panic("embedded favicon missing")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(icon)
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
	migrateNormalizedDatabaseStorage,
	func(ctx context.Context, tx *sql.Tx) error {
		return addColumnIfMissing(ctx, tx, "messages", "undo_sequence", `INTEGER CHECK (undo_sequence IS NULL OR undo_sequence > 0)`)
	},
	func(ctx context.Context, tx *sql.Tx) error {
		var tableCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM sqlite_master
			WHERE type = 'table' AND name = 'settings'
		`).Scan(&tableCount); err != nil {
			return fmt.Errorf("inspect settings table: %w", err)
		}
		if tableCount == 0 {
			return nil
		}
		if err := addColumnIfMissing(ctx, tx, "settings", "ntfy_endpoint", `TEXT`); err != nil {
			return err
		}
		if err := addColumnIfMissing(ctx, tx, "settings", "ntfy_topic", `TEXT`); err != nil {
			return err
		}
		return addColumnIfMissing(ctx, tx, "settings", "ntfy_api_key", `TEXT`)
	},
}

type legacyChatCollections struct {
	id      int64
	tools   []string
	appends []string
}

type legacyMessageCollections struct {
	id             int64
	toolCalls      []llm.ToolCall
	promptAppends  []string
	providerOutput []json.RawMessage
}

func migrateNormalizedDatabaseStorage(ctx context.Context, tx *sql.Tx) error {
	settingsValues := make(map[string]string)
	rows, err := tx.QueryContext(ctx, `SELECT name, value FROM settings`)
	if err != nil {
		return fmt.Errorf("read legacy settings: %w", err)
	}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy setting: %w", err)
		}
		settingsValues[name] = value
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy settings: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy settings: %w", err)
	}

	var defaultTools []string
	defaultToolsConfigured := false
	if encoded, ok := settingsValues["default_tools"]; ok {
		// The legacy getter treated malformed tool JSON as unset and returned
		// its caller-provided fallback.
		if err := json.Unmarshal([]byte(encoded), &defaultTools); err == nil {
			defaultToolsConfigured = true
		}
	}
	if defaultTools == nil {
		defaultTools = []string{}
	}

	var promptAppends []kritui_db.PromptAppend
	promptAppendsConfigured := false
	if encoded, ok := settingsValues["prompt_appends"]; ok {
		if err := json.Unmarshal([]byte(encoded), &promptAppends); err != nil {
			return fmt.Errorf("decode legacy prompt appends: %w", err)
		}
		if err := kritui_db.ValidatePromptAppends(promptAppends); err != nil {
			return fmt.Errorf("validate legacy prompt appends: %w", err)
		}
		promptAppendsConfigured = true
	}
	if promptAppends == nil {
		promptAppends = []kritui_db.PromptAppend{}
	}

	var chats []legacyChatCollections
	rows, err = tx.QueryContext(ctx, `SELECT id, tools, appends FROM chats ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read legacy chat collections: %w", err)
	}
	for rows.Next() {
		var chat legacyChatCollections
		var encodedTools, encodedAppends string
		if err := rows.Scan(&chat.id, &encodedTools, &encodedAppends); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy chat collections: %w", err)
		}
		if err := json.Unmarshal([]byte(encodedTools), &chat.tools); err != nil {
			rows.Close()
			return fmt.Errorf("decode tools for legacy chat %d: %w", chat.id, err)
		}
		if err := json.Unmarshal([]byte(encodedAppends), &chat.appends); err != nil {
			rows.Close()
			return fmt.Errorf("decode prompt appends for legacy chat %d: %w", chat.id, err)
		}
		if chat.tools == nil {
			chat.tools = []string{}
		}
		if chat.appends == nil {
			chat.appends = []string{}
		}
		chats = append(chats, chat)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy chat collections: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy chat collections: %w", err)
	}

	var messages []legacyMessageCollections
	rows, err = tx.QueryContext(ctx, `
		SELECT id, tool_calls, provider_metadata, prompt_appends
		FROM messages
		ORDER BY id
	`)
	if err != nil {
		return fmt.Errorf("read legacy message collections: %w", err)
	}
	for rows.Next() {
		var message legacyMessageCollections
		var encodedToolCalls, encodedProviderMetadata, encodedPromptAppends sql.NullString
		if err := rows.Scan(&message.id, &encodedToolCalls, &encodedProviderMetadata, &encodedPromptAppends); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy message collections: %w", err)
		}
		if encodedToolCalls.Valid {
			if err := json.Unmarshal([]byte(encodedToolCalls.String), &message.toolCalls); err != nil {
				rows.Close()
				return fmt.Errorf("decode tool calls for legacy message %d: %w", message.id, err)
			}
		}
		if encodedPromptAppends.Valid {
			if err := json.Unmarshal([]byte(encodedPromptAppends.String), &message.promptAppends); err != nil {
				rows.Close()
				return fmt.Errorf("decode prompt appends for legacy message %d: %w", message.id, err)
			}
		}
		if encodedProviderMetadata.Valid {
			var metadata llm.ProviderMetadata
			if err := json.Unmarshal([]byte(encodedProviderMetadata.String), &metadata); err != nil {
				rows.Close()
				return fmt.Errorf("decode provider metadata for legacy message %d: %w", message.id, err)
			}
			message.providerOutput = metadata.ResponsesOutput()
		}
		messages = append(messages, message)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy message collections: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy message collections: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS messages_touch_chat_after_insert;
		DROP TRIGGER IF EXISTS messages_touch_chat_after_update;
		DROP TRIGGER IF EXISTS messages_touch_chat_after_delete;
		DROP INDEX IF EXISTS messages_chat_created_at_idx;
		ALTER TABLE messages RENAME TO legacy_messages;
		ALTER TABLE chats RENAME TO legacy_chats;
		ALTER TABLE settings RENAME TO legacy_settings;
	`); err != nil {
		return fmt.Errorf("rename legacy tables: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create normalized schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chats (id, title, created_at, updated_at)
		SELECT id, title, created_at, updated_at FROM legacy_chats;

		INSERT INTO messages (id, chat_id, position, role, content, model, total_tokens, cost, tool_call_id, created_at)
		SELECT id, chat_id, position, role, content, model, total_tokens, cost, tool_call_id, created_at
		FROM legacy_messages;

		UPDATE chats
		SET updated_at = (SELECT updated_at FROM legacy_chats WHERE legacy_chats.id = chats.id);
	`); err != nil {
		return fmt.Errorf("copy normalized records: %w", err)
	}

	var defaultModel any
	if value, ok := settingsValues["default_model"]; ok {
		defaultModel = value
	}
	var maxToolRounds any
	if value, ok := settingsValues["max_tool_rounds"]; ok {
		if rounds, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil && rounds >= 1 && rounds <= kritui_db.MaxConfigurableToolRounds {
			maxToolRounds = rounds
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE settings
		SET default_model = ?, max_tool_rounds = ?, default_tools_configured = ?, prompt_appends_configured = ?
		WHERE id = 1
	`, defaultModel, maxToolRounds, defaultToolsConfigured, promptAppendsConfigured); err != nil {
		return fmt.Errorf("migrate typed settings: %w", err)
	}
	for position, name := range defaultTools {
		if _, err := tx.ExecContext(ctx, `INSERT INTO default_tools (position, name) VALUES (?, ?)`, position, name); err != nil {
			return fmt.Errorf("migrate default tool %d: %w", position, err)
		}
	}
	for position, value := range promptAppends {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompt_appends (id, position, name, text, enabled_by_default)
			VALUES (?, ?, ?, ?, ?)
		`, value.ID, position, value.Name, value.Text, value.EnabledByDefault); err != nil {
			return fmt.Errorf("migrate prompt append %q: %w", value.ID, err)
		}
	}
	for _, chat := range chats {
		for position, name := range chat.tools {
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_tools (chat_id, position, name) VALUES (?, ?, ?)`, chat.id, position, name); err != nil {
				return fmt.Errorf("migrate tool %d for chat %d: %w", position, chat.id, err)
			}
		}
		for position, id := range chat.appends {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chat_prompt_appends (chat_id, position, prompt_append_id)
				VALUES (?, ?, ?)
			`, chat.id, position, id); err != nil {
				return fmt.Errorf("migrate prompt append %d for chat %d: %w", position, chat.id, err)
			}
		}
	}
	for _, message := range messages {
		for position, call := range message.toolCalls {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO message_tool_calls
					(message_id, message_role, position, call_id, call_type, function_name, arguments)
				VALUES (?, 'assistant', ?, ?, ?, ?, ?)
			`, message.id, position, call.ID, call.Type, call.Function.Name, call.Function.Arguments); err != nil {
				return fmt.Errorf("migrate tool call %d for message %d: %w", position, message.id, err)
			}
		}
		for position, text := range message.promptAppends {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO message_prompt_appends (message_id, message_role, position, text)
				VALUES (?, 'user', ?, ?)
			`, message.id, position, text); err != nil {
				return fmt.Errorf("migrate prompt append %d for message %d: %w", position, message.id, err)
			}
		}
		for position, output := range message.providerOutput {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO message_provider_outputs (message_id, message_role, position, payload)
				VALUES (?, 'assistant', ?, ?)
			`, message.id, position, string(output)); err != nil {
				return fmt.Errorf("migrate provider output %d for message %d: %w", position, message.id, err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DROP TABLE legacy_messages;
		DROP TABLE legacy_chats;
		DROP TABLE legacy_settings;
	`); err != nil {
		return fmt.Errorf("remove legacy tables: %w", err)
	}
	return nil
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
