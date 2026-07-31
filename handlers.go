package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
)

func homeHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if chat == "" {
			chats, err := kritui_db.GetChats(r.Context(), database)
			if err != nil {
				log.Printf("get chats: %v", err)
				http.Error(w, "failed to get chats", http.StatusInternalServerError)
				return
			}

			nextChatID := int64(1)
			for _, chat := range chats {
				if chat.ID >= nextChatID {
					nextChatID = chat.ID + 1
				}
			}

			query := r.URL.Query()
			query.Set("chat", strconv.FormatInt(nextChatID, 10))
			target := *r.URL
			target.RawQuery = query.Encode()
			http.Redirect(w, r, target.String(), http.StatusSeeOther)
			return
		}

		chatID, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			http.Error(w, "failed to get messages", http.StatusInternalServerError)
			return
		}
		models, selectedModel := availableModels(r)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home(chat, messages, models, selectedModel).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
	}
}

func messageHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}

		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		model := strings.TrimSpace(r.FormValue("model"))
		if model == "" {
			model = os.Getenv("LLM_MODEL")
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{Role: "user", Content: message}
		if err := templates.PendingSubmission(strconv.FormatInt(chatID, 10), userMessage, model).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render pending message", http.StatusInternalServerError)
		}
	}
}

func messageCompletionHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}

		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		if _, err := database.ExecContext(r.Context(), `
			INSERT INTO chats (id) VALUES (?)
			ON CONFLICT (id) DO NOTHING
		`, chatID); err != nil {
			log.Printf("create chat: %v", err)
			http.Error(w, "failed to create chat", http.StatusInternalServerError)
			return
		}

		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			http.Error(w, "failed to get messages", http.StatusInternalServerError)
			return
		}

		model := strings.TrimSpace(r.FormValue("model"))
		if model == "" {
			model = os.Getenv("LLM_MODEL")
		}
		client, err := llm.New(os.Getenv("LLM_KEY"), model, os.Getenv("LLM_ENDPOINT"))
		if err != nil {
			log.Printf("configure llm: %v", err)
			http.Error(w, "failed to configure llm", http.StatusInternalServerError)
			return
		}

		userMessage := llm.Message{Role: "user", Content: message}
		position := len(messages)
		messages = append(messages, userMessage)
		completion, err := client.Complete(r.Context(), messages)
		if err != nil {
			log.Printf("complete message: %v", err)
			http.Error(w, "failed to complete message", http.StatusBadGateway)
			return
		}
		if _, err := kritui_db.InsertMessage(r.Context(), database, chatID, position, userMessage); err != nil {
			log.Printf("store user message: %v", err)
			http.Error(w, "failed to store message", http.StatusInternalServerError)
			return
		}
		if _, err := kritui_db.InsertMessage(r.Context(), database, chatID, position+1, completion.Message); err != nil {
			log.Printf("store assistant message: %v", err)
			http.Error(w, "failed to store message", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Messages(completion.Message).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render message", http.StatusInternalServerError)
		}
	}
}

func availableModels(r *http.Request) ([]string, string) {
	selected := strings.TrimSpace(os.Getenv("LLM_MODEL"))
	client, err := llm.New(os.Getenv("LLM_KEY"), selected, os.Getenv("LLM_ENDPOINT"))
	if err != nil {
		if selected == "" {
			return nil, ""
		}
		return []string{selected}, selected
	}

	models, err := client.Models(r.Context())
	if err != nil {
		log.Printf("get models: %v", err)
		return []string{selected}, selected
	}
	for _, model := range models {
		if model == selected {
			return models, selected
		}
	}
	return append([]string{selected}, models...), selected
}
