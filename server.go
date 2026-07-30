package main

import (
	"embed"
	"log"
	"net/http"
)

//go:embed static
var staticFiles embed.FS

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", homeHandler)
	mux.HandleFunc("POST /messages", messageHandler)
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFiles)))

	log.Println("listening on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
