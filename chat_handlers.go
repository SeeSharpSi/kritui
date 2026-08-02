package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/tools"
)

func homeHandler(database *sql.DB, registry *tools.Registry) http.HandlerFunc {
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

		chatID, ok := positiveID(chat)
		if !ok {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			http.Error(w, "failed to get messages", http.StatusInternalServerError)
			return
		}
		enabledTools, err := kritui_db.GetChatTools(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get chat tools: %v", err)
			http.Error(w, "failed to get chat tools", http.StatusInternalServerError)
			return
		}
		selectedModel, err := kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			http.Error(w, "failed to get settings", http.StatusInternalServerError)
			return
		}
		for index := len(messages) - 1; index >= 0; index-- {
			if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Model) != "" {
				selectedModel = messages[index].Model
				break
			}
		}
		models, selectedModel := availableModels(r, selectedModel)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home(chat, messages, models, selectedModel, registry.Names(), enabledTools).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
	}
}

func historyHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if _, ok := positiveID(chat); !ok {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Has("close") {
			if err := templates.HistoryClose(chat).Render(r.Context(), w); err != nil {
				http.Error(w, "failed to close chat history", http.StatusInternalServerError)
			}
			return
		}

		renderHistoryList(r.Context(), w, database, chat)
	}
}

func settingsHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if _, ok := positiveID(chat); !ok {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Has("close") {
			if err := templates.SettingsClose(chat).Render(r.Context(), w); err != nil {
				http.Error(w, "failed to close settings", http.StatusInternalServerError)
			}
			return
		}

		selectedModel, err := kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			http.Error(w, "failed to get settings", http.StatusInternalServerError)
			return
		}
		saved := false
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			selectedModel = strings.TrimSpace(r.FormValue("model"))
			if selectedModel == "" {
				http.Error(w, "model is required", http.StatusBadRequest)
				return
			}
			if err := kritui_db.SetDefaultModel(r.Context(), database, selectedModel); err != nil {
				log.Printf("set default model: %v", err)
				http.Error(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
			saved = true
		}

		models, selectedModel := availableModels(r, selectedModel)
		if err := templates.SettingsPage(chat, models, selectedModel, saved).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render settings", http.StatusInternalServerError)
		}
	}
}

func deleteChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		current := r.URL.Query().Get("current")
		currentChatID, ok := positiveID(current)
		if !ok {
			http.Error(w, "valid current chat is required", http.StatusBadRequest)
			return
		}

		if _, err := database.ExecContext(r.Context(), `DELETE FROM chats WHERE id = ?`, chatID); err != nil {
			log.Printf("delete chat: %v", err)
			http.Error(w, "failed to delete chat", http.StatusInternalServerError)
			return
		}
		renderHistoryList(r.Context(), w, database, current)
		if chatID == currentChatID {
			if err := templates.MessageList(nil, true).Render(r.Context(), w); err != nil {
				log.Printf("clear deleted chat: %v", err)
			}
		}
	}
}

func renameChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		current := r.URL.Query().Get("current")
		if _, ok := positiveID(current); !ok {
			http.Error(w, "valid current chat is required", http.StatusBadRequest)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		title := strings.TrimSpace(r.FormValue("title"))
		if title == "" {
			http.Error(w, "title is required", http.StatusBadRequest)
			return
		}

		result, err := database.ExecContext(r.Context(), `
			UPDATE chats
			SET title = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?
		`, title, chatID)
		if err != nil {
			log.Printf("rename chat: %v", err)
			http.Error(w, "failed to rename chat", http.StatusInternalServerError)
			return
		}
		affected, err := result.RowsAffected()
		if err != nil {
			log.Printf("rename chat rows affected: %v", err)
			http.Error(w, "failed to rename chat", http.StatusInternalServerError)
			return
		}
		if affected == 0 {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}

		renderHistoryList(r.Context(), w, database, current)
	}
}

func renderHistoryList(ctx context.Context, w http.ResponseWriter, database *sql.DB, current string) {
	chats, err := kritui_db.GetChats(ctx, database)
	if err != nil {
		log.Printf("get chats: %v", err)
		http.Error(w, "failed to get chats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.HistoryList(current, chats).Render(ctx, w); err != nil {
		http.Error(w, "failed to render chat history", http.StatusInternalServerError)
	}
}

func positiveID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func availableModels(r *http.Request, selected string) ([]string, string) {
	selected = strings.TrimSpace(selected)
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
	if slices.Contains(models, selected) {
		return models, selected
	}
	if selected == "" {
		return models, selected
	}
	return append([]string{selected}, models...), selected
}
