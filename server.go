package main

import (
	"database/sql"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"seesharpsi/kritui/tools"

	_ "modernc.org/sqlite"
)

//go:embed static
var staticFiles embed.FS

//go:embed db/schema.sql
var schema string

func main() {
	databaseExists := true
	if _, err := os.Stat("data.db"); errors.Is(err, os.ErrNotExist) {
		databaseExists = false
	} else if err != nil {
		log.Fatalf("check database: %v", err)
	}

	database, err := sql.Open("sqlite", "file:data.db?_pragma=foreign_keys(1)")
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
	mux.HandleFunc("GET /{$}", homeHandler(database, toolRegistry))
	mux.HandleFunc("GET /history", historyHandler(database))
	mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	mux.HandleFunc("POST /messages", messageHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/retry", messageRetryHandler(database, toolRegistry, toolCalls))
	mux.HandleFunc("POST /messages/complete", messageCompletionHandler(database, toolRegistry, toolCalls, toolCallLogger))
	mux.HandleFunc("GET /messages/tools", messageToolStreamHandler(toolCalls))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFiles)))

	log.Println("listening on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}

func migrateDatabase(database *sql.DB) error {
	for _, statement := range []string{
		`ALTER TABLE messages ADD COLUMN total_tokens INTEGER`,
		`ALTER TABLE messages ADD COLUMN cost REAL`,
	} {
		if _, err := database.Exec(statement); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}
