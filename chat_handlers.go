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

const (
	defaultHistoryPageSize = 10
	maxHistoryPageSize     = 50
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
		defaultModel, err := kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			http.Error(w, "failed to get settings", http.StatusInternalServerError)
			return
		}
		selectedModel := defaultModel
		for index := len(messages) - 1; index >= 0; index-- {
			if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Model) != "" {
				selectedModel = messages[index].Model
				break
			}
		}
		models, selectedModel := availableModels(r, selectedModel)
		if defaultModel != "" && !slices.Contains(models, defaultModel) {
			models = append([]string{defaultModel}, models...)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home(chat, messages, models, selectedModel, defaultModel, registry.Names(), enabledTools).Render(r.Context(), w); err != nil {
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
		beforeUpdatedAt := r.URL.Query().Get("before")
		beforeID, ok := historyCursorID(r, beforeUpdatedAt)
		if !ok {
			http.Error(w, "valid history cursor is required", http.StatusBadRequest)
			return
		}
		limit, ok := historyPageLimit(r)
		if !ok {
			http.Error(w, "valid history limit is required", http.StatusBadRequest)
			return
		}
		renderHistoryEntries(r.Context(), w, database, chat, beforeUpdatedAt, beforeID, limit)
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
		if err := templates.SettingsPage(chat, models, selectedModel, saved, true).Render(r.Context(), w); err != nil {
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
		renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize)
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

		renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize)
	}
}

func renderHistoryEntries(ctx context.Context, w http.ResponseWriter, database *sql.DB, current, beforeUpdatedAt string, beforeID int64, limit int) {
	chats, err := kritui_db.GetChatsPage(ctx, database, beforeUpdatedAt, beforeID, limit+1)
	if err != nil {
		log.Printf("get chats: %v", err)
		http.Error(w, "failed to get chats", http.StatusInternalServerError)
		return
	}
	hasMore := len(chats) > limit
	if hasMore {
		chats = chats[:limit]
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.HistoryEntries(current, chats, beforeUpdatedAt == "", hasMore).Render(ctx, w); err != nil {
		http.Error(w, "failed to render chat history", http.StatusInternalServerError)
	}
}

func historyCursorID(r *http.Request, beforeUpdatedAt string) (int64, bool) {
	value := r.URL.Query().Get("before_id")
	if beforeUpdatedAt == "" {
		return 0, value == ""
	}
	return positiveID(value)
}

func historyPageLimit(r *http.Request) (int, bool) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultHistoryPageSize, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && limit > 0 && limit <= maxHistoryPageSize
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
