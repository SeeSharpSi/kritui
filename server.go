package main

import (
	"database/sql"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"

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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", homeHandler(database))
	mux.HandleFunc("POST /messages", messageHandler(database))
	mux.HandleFunc("POST /messages/complete", messageCompletionHandler(database))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFiles)))

	log.Println("listening on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
