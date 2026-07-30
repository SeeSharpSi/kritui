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
		if r.URL.Query().Get("chat") == "" {
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

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home().Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
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
