package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"seesharpsi/kritui/llm"
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

	client, err := llm.New(os.Getenv("LLM_KEY"), os.Getenv("LLM_MODEL"), os.Getenv("LLM_ENDPOINT"))
	if err != nil {
		log.Printf("configure llm: %v", err)
		http.Error(w, "failed to configure llm", http.StatusInternalServerError)
		return
	}

	userMessage := llm.Message{Role: "user", Content: message}
	completion, err := client.Complete(r.Context(), []llm.Message{userMessage})
	if err != nil {
		log.Printf("complete message: %v", err)
		http.Error(w, "failed to complete message", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.Messages(userMessage, completion.Message).Render(r.Context(), w); err != nil {
		http.Error(w, "failed to render message", http.StatusInternalServerError)
	}
}
