package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/tools"
)

type messageRequest struct {
	chat     string
	chatID   int64
	model    string
	selected *tools.Registry
}

func messageHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			renderMessageError(w, r, http.StatusBadRequest, "Message is required.")
			return
		}

		request, ok := parseMessageRequest(w, r, registry)
		if !ok {
			return
		}
		requestID, err := toolCalls.create(request.chatID)
		if err != nil {
			if errors.Is(err, errChatCompletionActive) {
				renderMessageError(w, r, http.StatusConflict, "A response is already in progress.")
				return
			}
			log.Printf("create tool-call tracker: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to prepare message.")
			return
		}
		if err := persistUserMessage(r.Context(), database, request.chatID, message, request.selected.Names()); err != nil {
			toolCalls.delete(requestID)
			log.Printf("store user message: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to store message.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{Role: "user", Content: message}
		if err := templates.PendingSubmission(strconv.FormatInt(request.chatID, 10), requestID, userMessage, request.model, request.selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render pending message: %v", err)
		}
	}
}

func messageCompletionHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore, toolCallLogger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, ok := parseMessageRequest(w, r, registry)
		if !ok {
			return
		}

		requestID := strings.TrimSpace(r.FormValue("request"))
		tracker, ok := toolCalls.claim(requestID, request.chatID)
		if !ok {
			renderMessageError(w, r, http.StatusBadRequest, "This completion request is no longer valid.")
			return
		}
		defer toolCalls.delete(requestID)

		messages, err := kritui_db.GetMessages(r.Context(), database, request.chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to load conversation.", request.model, request.selected.Names())
			return
		}
		if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}
		client, err := llm.New(os.Getenv("LLM_KEY"), request.model, os.Getenv("LLM_ENDPOINT"))
		if err != nil {
			log.Printf("configure llm: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to configure model.", request.model, request.selected.Names())
			return
		}

		position := len(messages)
		conversation, err := llm.NewConversation(client, request.selected, llm.PromptContext{
			CurrentTime:    time.Now(),
			ClientLocation: clientLocation(r.FormValue("client_timezone")),
		}, messages...)
		if err != nil {
			log.Printf("configure conversation: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to configure conversation.", request.model, request.selected.Names())
			return
		}
		conversation.SetToolCallLogger(toolCallLogger)
		conversation.SetToolCallObserver(tracker.observe)
		completion, err := conversation.Complete(r.Context())
		if err != nil {
			log.Printf("complete message: %v", err)
			message := completionErrorMessage(err)
			renderCompletionError(w, r, http.StatusFailedDependency, request.chat, message, request.model, request.selected.Names())
			return
		}
		completedMessages := conversation.Messages()[len(messages)+1:]
		tx, err := database.BeginTx(r.Context(), nil)
		if err != nil {
			log.Printf("begin message transaction: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to store response.", request.model, request.selected.Names())
			return
		}
		defer tx.Rollback()
		for index, completedMessage := range completedMessages {
			if _, err := kritui_db.InsertMessage(r.Context(), tx, request.chatID, position+index, completedMessage); err != nil {
				log.Printf("store message: %v", err)
				renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to store response.", request.model, request.selected.Names())
				return
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("commit messages: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to store response.", request.model, request.selected.Names())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		calls, _ := tracker.snapshot()
		if err := templates.CompletedMessage(calls, completion.Message).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render message", http.StatusInternalServerError)
		}
	}
}

func messageRetryHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, ok := parseMessageRequest(w, r, registry)
		if !ok {
			return
		}

		var role string
		err := database.QueryRowContext(r.Context(), `
			SELECT role
			FROM messages
			WHERE chat_id = ?
			ORDER BY position DESC
			LIMIT 1
		`, request.chatID).Scan(&role)
		if err == sql.ErrNoRows {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}
		if err != nil {
			log.Printf("get retry message: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to prepare retry.", request.model, request.selected.Names())
			return
		}
		if role != "user" {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}

		requestID, err := toolCalls.create(request.chatID)
		if err != nil {
			if errors.Is(err, errChatCompletionActive) {
				renderCompletionError(w, r, http.StatusConflict, request.chat, "A response is already in progress.", request.model, request.selected.Names())
				return
			}
			log.Printf("create retry tracker: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to prepare retry.", request.model, request.selected.Names())
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.PendingCompletion(request.chat, requestID, request.model, request.selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render retry: %v", err)
		}
	}
}

func clientLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return location
}

func parseMessageRequest(w http.ResponseWriter, r *http.Request, registry *tools.Registry) (messageRequest, bool) {
	chat := r.URL.Query().Get("chat")
	chatID, ok := positiveID(chat)
	if !ok {
		renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
		return messageRequest{}, false
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		model = os.Getenv("LLM_MODEL")
	}
	selected, err := registry.Select(r.Form["tool"]...)
	if err != nil {
		renderMessageError(w, r, http.StatusBadRequest, "Tool selection is invalid.")
		return messageRequest{}, false
	}
	return messageRequest{chat: chat, chatID: chatID, model: model, selected: selected}, true
}

func persistUserMessage(ctx context.Context, database *sql.DB, chatID int64, message string, tools []string) error {
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("encode chat tools: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user message transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chats (id, title, tools) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			title = CASE WHEN chats.title = '' THEN excluded.title ELSE chats.title END,
			tools = excluded.tools
	`, chatID, message, string(toolsJSON)); err != nil {
		return fmt.Errorf("create chat: %w", err)
	}

	var position int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(position) + 1, 0)
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&position); err != nil {
		return fmt.Errorf("get user message position: %w", err)
	}
	if _, err := kritui_db.InsertMessage(ctx, tx, chatID, position, llm.Message{Role: "user", Content: message}); err != nil {
		return fmt.Errorf("insert user message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user message: %w", err)
	}
	return nil
}

func renderMessageError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.MessageError(message).Render(r.Context(), w); err != nil {
		log.Printf("render message error: %v", err)
	}
}

func completionErrorMessage(err error) string {
	const fallback = "Failed to complete message."
	const toolCallLimit = "llm: exceeded 16 consecutive tool-call rounds"

	if err == nil {
		return fallback
	}
	if errors.Is(err, context.Canceled) {
		return "Completion request was canceled."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Completion request timed out."
	}
	var apiError *llm.APIError
	if errors.As(err, &apiError) && apiError != nil {
		status := http.StatusText(apiError.StatusCode)
		if status == "" {
			return "Model endpoint returned an error."
		}
		return fmt.Sprintf("Model endpoint returned HTTP %d: %s.", apiError.StatusCode, status)
	}
	if err.Error() == toolCallLimit {
		return toolCallLimit
	}
	return fallback
}

func renderCompletionError(w http.ResponseWriter, r *http.Request, status int, chatID string, message string, model string, tools []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.CompletionError(chatID, message, model, tools).Render(r.Context(), w); err != nil {
		log.Printf("render completion error: %v", err)
	}
}
