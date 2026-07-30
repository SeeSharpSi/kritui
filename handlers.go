package main

import (
	"net/http"
	"strings"

	"seesharpsi/kritui/templ"
)

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
