package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
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
			chatID, err := kritui_db.AllocateChat(r.Context(), database)
			if err != nil {
				log.Printf("allocate chat: %v", err)
				renderPageError(w, r, http.StatusInternalServerError, "Failed to allocate chat.")
				return
			}

			query := r.URL.Query()
			query.Set("chat", strconv.FormatInt(chatID, 10))
			target := *r.URL
			target.RawQuery = query.Encode()
			http.Redirect(w, r, target.String(), http.StatusSeeOther)
			return
		}

		chatID, ok := positiveID(chat)
		if !ok {
			renderPageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load messages.")
			return
		}
		enabledTools, err := kritui_db.GetChatTools(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get chat tools: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load chat tools.")
			return
		}
		defaultModel, err := kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		maxToolRounds, err := kritui_db.GetMaxToolRounds(r.Context(), database, llm.DefaultMaxToolCallRounds)
		if err != nil {
			log.Printf("get max tool rounds: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
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

		var page bytes.Buffer
		if err := templates.Home(chat, messages, models, selectedModel, defaultModel, maxToolRounds, registry.Names(), enabledTools).Render(r.Context(), &page); err != nil {
			log.Printf("render page: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to render page.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page.Bytes())
	}
}

func historyHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if _, ok := positiveID(chat); !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		beforeUpdatedAt := r.URL.Query().Get("before")
		beforeID, ok := historyCursorID(r, beforeUpdatedAt)
		if !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid history cursor is required.")
			return
		}
		limit, ok := historyPageLimit(r)
		if !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid history limit is required.")
			return
		}
		if err := renderHistoryEntries(r.Context(), w, database, chat, beforeUpdatedAt, beforeID, limit); err != nil {
			log.Printf("render chat history: %v", err)
			renderHistoryLoadError(w, r, http.StatusInternalServerError, "Failed to load chat history.")
		}
	}
}

func settingsHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if _, ok := positiveID(chat); !ok {
			renderSettingsPage(w, r, http.StatusBadRequest, chat, os.Getenv("LLM_MODEL"), llm.DefaultMaxToolCallRounds, false, "A valid chat is required.")
			return
		}

		selectedModel, err := kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			renderSettingsPage(w, r, http.StatusInternalServerError, chat, os.Getenv("LLM_MODEL"), llm.DefaultMaxToolCallRounds, false, "Failed to load settings.")
			return
		}
		maxToolRounds, err := kritui_db.GetMaxToolRounds(r.Context(), database, llm.DefaultMaxToolCallRounds)
		if err != nil {
			log.Printf("get max tool rounds: %v", err)
			renderSettingsPage(w, r, http.StatusInternalServerError, chat, selectedModel, llm.DefaultMaxToolCallRounds, false, "Failed to load settings.")
			return
		}
		saved := false
		if r.Method == http.MethodPost {
			if err := parseLimitedForm(w, r, maxSettingsBodyBytes); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					renderSettingsPage(w, r, http.StatusRequestEntityTooLarge, chat, selectedModel, maxToolRounds, false, "Request body is too large.")
					return
				}
				renderSettingsPage(w, r, http.StatusBadRequest, chat, selectedModel, maxToolRounds, false, "Invalid settings form.")
				return
			}
			selectedModel = strings.TrimSpace(r.FormValue("model"))
			if selectedModel == "" {
				renderSettingsPage(w, r, http.StatusBadRequest, chat, selectedModel, maxToolRounds, false, "A model is required.")
				return
			}
			submittedRounds, parseErr := strconv.Atoi(strings.TrimSpace(r.FormValue("max_tool_rounds")))
			if parseErr != nil || submittedRounds < 1 || submittedRounds > kritui_db.MaxConfigurableToolRounds {
				renderSettingsPage(w, r, http.StatusBadRequest, chat, selectedModel, maxToolRounds, false, fmt.Sprintf("Max tool-call rounds must be between 1 and %d.", kritui_db.MaxConfigurableToolRounds))
				return
			}
			maxToolRounds = submittedRounds
			if err := kritui_db.SetDefaultModel(r.Context(), database, selectedModel); err != nil {
				log.Printf("set default model: %v", err)
				renderSettingsPage(w, r, http.StatusInternalServerError, chat, selectedModel, maxToolRounds, false, "Failed to save settings.")
				return
			}
			if err := kritui_db.SetMaxToolRounds(r.Context(), database, maxToolRounds); err != nil {
				log.Printf("set max tool rounds: %v", err)
				renderSettingsPage(w, r, http.StatusInternalServerError, chat, selectedModel, maxToolRounds, false, "Failed to save settings.")
				return
			}
			saved = true
		}

		renderSettingsPage(w, r, http.StatusOK, chat, selectedModel, maxToolRounds, saved, "")
	}
}

func deleteChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		current := r.URL.Query().Get("current")
		currentChatID, ok := positiveID(current)
		if !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid current chat is required.")
			return
		}

		if _, err := database.ExecContext(r.Context(), `DELETE FROM chats WHERE id = ?`, chatID); err != nil {
			log.Printf("delete chat: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to delete chat.")
			return
		}
		if err := renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize); err != nil {
			log.Printf("render chat history after delete: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Chat was deleted, but history could not be refreshed.")
			return
		}
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
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		current := r.URL.Query().Get("current")
		if _, ok := positiveID(current); !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid current chat is required.")
			return
		}

		if err := parseLimitedForm(w, r, maxRenameBodyBytes); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				renderHistoryMutationError(w, r, http.StatusRequestEntityTooLarge, "Request body is too large.")
				return
			}
			renderHistoryMutationError(w, r, http.StatusBadRequest, "Invalid rename form.")
			return
		}
		title := normalizeChatTitle(r.FormValue("title"))
		if title == "" {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A title is required.")
			return
		}

		result, err := database.ExecContext(r.Context(), `
			UPDATE chats
			SET title = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?
		`, title, chatID)
		if err != nil {
			log.Printf("rename chat: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to rename chat.")
			return
		}
		affected, err := result.RowsAffected()
		if err != nil {
			log.Printf("rename chat rows affected: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to rename chat.")
			return
		}
		if affected == 0 {
			renderHistoryMutationError(w, r, http.StatusNotFound, "Chat not found.")
			return
		}

		if err := renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize); err != nil {
			log.Printf("render chat history after rename: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Chat was renamed, but history could not be refreshed.")
		}
	}
}

func renderHistoryEntries(ctx context.Context, w http.ResponseWriter, database *sql.DB, current, beforeUpdatedAt string, beforeID int64, limit int) error {
	chats, err := kritui_db.GetChatsPage(ctx, database, beforeUpdatedAt, beforeID, limit+1)
	if err != nil {
		return fmt.Errorf("get chats: %w", err)
	}
	hasMore := len(chats) > limit
	if hasMore {
		chats = chats[:limit]
	}

	var fragment bytes.Buffer
	if err := templates.HistoryEntries(current, chats, beforeUpdatedAt == "", hasMore).Render(ctx, &fragment); err != nil {
		return fmt.Errorf("render history entries: %w", err)
	}
	if err := templates.HistoryError("", true).Render(ctx, &fragment); err != nil {
		return fmt.Errorf("render history status: %w", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(fragment.Bytes()); err != nil {
		return fmt.Errorf("write history entries: %w", err)
	}
	return nil
}

func renderPageError(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := templates.PageError(message).Render(r.Context(), &page); err != nil {
		log.Printf("render page error: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

func renderSettingsPage(w http.ResponseWriter, r *http.Request, status int, chat, selectedModel string, maxToolRounds int, saved bool, errorMessage string) {
	models, selectedModel := availableModels(r, selectedModel)
	var page bytes.Buffer
	if err := templates.SettingsPage(chat, models, selectedModel, maxToolRounds, saved, true, errorMessage).Render(r.Context(), &page); err != nil {
		log.Printf("render settings: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

func renderHistoryLoadError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.HistoryLoadError(message).Render(r.Context(), w); err != nil {
		log.Printf("render history load error: %v", err)
	}
}

func renderHistoryMutationError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Retarget", "#history-error")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.WriteHeader(status)
	if err := templates.HistoryError(message, false).Render(r.Context(), w); err != nil {
		log.Printf("render history mutation error: %v", err)
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
