package main

import (
	"embed"
	"log"
	"net/http"
	"strings"

	"seesharpsi/kritui/templ"
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

func homeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.Home().Render(r.Context(), w); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func messageHandler(w http.ResponseWriter, r *http.Request) {
	message := strings.TrimSpace(r.FormValue("message"))
	if message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.Message("user", message).Render(r.Context(), w); err != nil {
		http.Error(w, "failed to render message", http.StatusInternalServerError)
	}
}
