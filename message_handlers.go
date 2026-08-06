package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
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

const (
	maxMessageBodyBytes    int64 = 1 << 20
	maxCompletionBodyBytes int64 = 16 << 10
	maxRetryBodyBytes      int64 = 16 << 10
	// accommodates 32 prompt appends of 16 KiB each plus form overhead
	maxSettingsBodyBytes int64 = 1 << 20
	maxRenameBodyBytes   int64 = 16 << 10
	maxChatTitleRunes          = 120
)

func messageHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseMessageForm(w, r, maxMessageBodyBytes) {
			return
		}
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			renderMessageError(w, r, http.StatusBadRequest, "Message is required.")
			return
		}

		request, ok := parseMessageRequest(w, r, database, registry)
		if !ok {
			return
		}
		var selectedAppends []kritui_db.PromptAppend
		if appendIDs := r.Form["append"]; len(appendIDs) > 0 {
			promptAppends, err := kritui_db.GetPromptAppends(r.Context(), database)
			if err != nil {
				log.Printf("get prompt appends: %v", err)
				renderMessageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
				return
			}
			selectedAppends, err = kritui_db.SelectPromptAppends(promptAppends, appendIDs)
			if err != nil {
				renderMessageError(w, r, http.StatusBadRequest, "Prompt append selection is invalid.")
				return
			}
		}
		appendTexts := promptAppendTexts(selectedAppends)
		expandedMessage := appendPromptText(message, appendTexts)
		if int64(len([]byte(expandedMessage))) > maxMessageBodyBytes {
			renderMessageError(w, r, http.StatusRequestEntityTooLarge, "Message with prompt appends is too large.")
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
		if err := persistUserMessage(r.Context(), database, request.chatID, message, request.selected.Names(), selectedAppends); err != nil {
			toolCalls.delete(requestID)
			log.Printf("store user message: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to store message.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{Role: "user", Content: message, PromptAppendTexts: appendTexts}
		if err := templates.PendingSubmission(strconv.FormatInt(request.chatID, 10), requestID, userMessage, request.model, request.selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render pending message: %v", err)
		}
	}
}

func messageCompletionHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore, toolCallLogger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseMessageForm(w, r, maxCompletionBodyBytes) {
			return
		}
		request, ok := parseMessageRequest(w, r, database, registry)
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

		snapshot, err := kritui_db.GetMessageSnapshot(r.Context(), database, request.chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to load conversation.", request.model, request.selected.Names())
			return
		}
		messages := snapshot.Messages
		if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}
		conversationMessages := messagesWithPromptAppendTexts(messages)
		client, err := llm.New(os.Getenv("LLM_KEY"), request.model, os.Getenv("LLM_ENDPOINT"))
		if err != nil {
			log.Printf("configure llm: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to configure model.", request.model, request.selected.Names())
			return
		}

		conversation, err := llm.NewConversation(client, request.selected, llm.PromptContext{
			CurrentTime:    time.Now(),
			ClientLocation: clientLocation(r.FormValue("client_timezone")),
		}, conversationMessages...)
		if err != nil {
			log.Printf("configure conversation: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to configure conversation.", request.model, request.selected.Names())
			return
		}
		maxToolRounds, err := kritui_db.GetMaxToolRounds(r.Context(), database, llm.DefaultMaxToolCallRounds)
		if err != nil {
			log.Printf("get max tool rounds: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to load settings.", request.model, request.selected.Names())
			return
		}
		conversation.SetMaxToolRounds(maxToolRounds)
		conversation.SetToolCallLogger(toolCallLogger)
		conversation.SetToolCallObserver(tracker.observe)
		if _, err := conversation.Complete(r.Context()); err != nil {
			log.Printf("complete message: %v", err)
			message := completionErrorMessage(err)
			renderCompletionError(w, r, http.StatusFailedDependency, request.chat, message, request.model, request.selected.Names())
			return
		}
		completedMessages := conversation.Messages()[len(messages)+1:]
		if err := kritui_db.AppendCompletion(r.Context(), database, request.chatID, snapshot, completedMessages); err != nil {
			switch {
			case errors.Is(err, kritui_db.ErrConversationConflict):
				renderCompletionError(w, r, http.StatusConflict, request.chat, "Conversation changed while the response was being generated. Retry the completion.", request.model, request.selected.Names())
			case errors.Is(err, kritui_db.ErrChatNotFound):
				renderMessageError(w, r, http.StatusNotFound, "Chat no longer exists.")
			default:
				log.Printf("store response: %v", err)
				renderCompletionError(w, r, http.StatusInternalServerError, request.chat, "Failed to store response.", request.model, request.selected.Names())
			}
			return
		}
		var fragment bytes.Buffer
		if err := templates.CompletedMessage(completedMessages...).Render(r.Context(), &fragment); err != nil {
			log.Printf("render completed message: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Response completed, but could not be displayed.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(fragment.Bytes())
	}
}

func messageRetryHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseMessageForm(w, r, maxRetryBodyBytes) {
			return
		}
		request, ok := parseMessageRequest(w, r, database, registry)
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

func parseMessageRequest(w http.ResponseWriter, r *http.Request, database *sql.DB, registry *tools.Registry) (messageRequest, bool) {
	chat := r.URL.Query().Get("chat")
	chatID, ok := positiveID(chat)
	if !ok {
		renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
		return messageRequest{}, false
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		var err error
		model, err = kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
			return messageRequest{}, false
		}
	}
	selected, err := registry.Select(r.Form["tool"]...)
	if err != nil {
		renderMessageError(w, r, http.StatusBadRequest, "Tool selection is invalid.")
		return messageRequest{}, false
	}
	return messageRequest{chat: chat, chatID: chatID, model: model, selected: selected}, true
}

func persistUserMessage(ctx context.Context, database *sql.DB, chatID int64, message string, tools []string, selectedAppends []kritui_db.PromptAppend) error {
	if tools == nil {
		tools = []string{}
	}
	if selectedAppends == nil {
		selectedAppends = []kritui_db.PromptAppend{}
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("encode chat tools: %w", err)
	}
	appendIDsJSON, err := json.Marshal(kritui_db.PromptAppendIDs(selectedAppends))
	if err != nil {
		return fmt.Errorf("encode chat prompt append IDs: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user message transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chats (id, title, tools, appends) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			title = CASE WHEN chats.title = '' THEN excluded.title ELSE chats.title END,
			tools = excluded.tools,
			appends = excluded.appends
	`, chatID, normalizeChatTitle(message), string(toolsJSON), string(appendIDsJSON)); err != nil {
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
	if _, err := kritui_db.InsertMessage(ctx, tx, chatID, position, llm.Message{
		Role:              "user",
		Content:           message,
		PromptAppendTexts: promptAppendTexts(selectedAppends),
	}); err != nil {
		return fmt.Errorf("insert user message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user message: %w", err)
	}
	return nil
}

func appendPromptText(message string, texts []string) string {
	if len(texts) == 0 {
		return message
	}

	var builder strings.Builder
	builder.WriteString(message)
	for _, text := range texts {
		builder.WriteString("\n\n")
		builder.WriteString(text)
	}
	return builder.String()
}

func messagesWithPromptAppendTexts(messages []llm.Message) []llm.Message {
	expanded := make([]llm.Message, len(messages))
	for index, message := range messages {
		expanded[index] = message
		if message.Role == "user" && len(message.PromptAppendTexts) > 0 {
			expanded[index].Content = appendPromptText(message.Content, message.PromptAppendTexts)
		}
	}
	return expanded
}

func promptAppendTexts(values []kritui_db.PromptAppend) []string {
	texts := make([]string, 0, len(values))
	for _, value := range values {
		texts = append(texts, value.Text)
	}
	return texts
}

func parseLimitedForm(w http.ResponseWriter, r *http.Request, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return err
		}
		if mediaType == "multipart/form-data" {
			return r.ParseMultipartForm(limit)
		}
	}
	return r.ParseForm()
}

func parseMessageForm(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if err := parseLimitedForm(w, r, limit); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			renderMessageError(w, r, http.StatusRequestEntityTooLarge, "Request body is too large.")
			return false
		}
		renderMessageError(w, r, http.StatusBadRequest, "Invalid message form.")
		return false
	}
	return true
}

func normalizeChatTitle(value string) string {
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > maxChatTitleRunes {
			runes = runes[:maxChatTitleRunes]
		}
		return string(runes)
	}
	return ""
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
	var limitError *llm.MaxToolRoundsError
	if errors.As(err, &limitError) {
		return limitError.Error()
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
