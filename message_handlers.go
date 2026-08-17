package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"seesharpsi/kritui/commands"
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

type persistedUserMessage struct {
	id      int64
	appends []kritui_db.PromptAppend
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

var (
	// errInvalidAppendSelection means a submitted prompt-append ID is unknown
	// or repeated against the definitions read inside the message transaction.
	errInvalidAppendSelection = errors.New("prompt append selection is invalid")
	// errPromptAppendTooLarge means the expanded message exceeds the body limit.
	errPromptAppendTooLarge = errors.New("message with prompt appends is too large")
)

func messageHandler(database *sql.DB, registry *tools.Registry, commandRegistry *commands.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseMessageForm(w, r, maxMessageBodyBytes) {
			return
		}
		rawMessage := r.FormValue("message")
		parsedCommand, isCommand, err := commands.Parse(rawMessage)
		if isCommand {
			if err != nil {
				renderMessageError(w, r, http.StatusBadRequest, "Invalid slash command.")
				return
			}
			chatID, ok := positiveID(r.URL.Query().Get("chat"))
			if !ok {
				renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
				return
			}
			result, err := commandRegistry.Execute(r.Context(), parsedCommand, chatID)
			if err != nil {
				renderCommandError(w, r, parsedCommand.Name, err)
				return
			}
			renderCommandResult(w, r, result)
			return
		}

		message := strings.TrimSpace(rawMessage)
		if message == "" {
			renderMessageError(w, r, http.StatusBadRequest, "Message is required.")
			return
		}

		request, ok := parseMessageRequest(w, r, database, registry)
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
		persisted, err := persistUserMessage(r.Context(), database, request.chatID, message, request.selected.Names(), r.Form["append"])
		if err != nil {
			toolCalls.delete(requestID)
			switch {
			case errors.Is(err, errInvalidAppendSelection):
				renderMessageError(w, r, http.StatusBadRequest, "Prompt append selection is invalid.")
			case errors.Is(err, errPromptAppendTooLarge):
				renderMessageError(w, r, http.StatusRequestEntityTooLarge, "Message with prompt appends is too large.")
			default:
				log.Printf("store user message: %v", err)
				renderMessageError(w, r, http.StatusInternalServerError, "Failed to store message.")
			}
			return
		}

		appendTexts := promptAppendTexts(persisted.appends)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{ID: persisted.id, Role: "user", Content: message, PromptAppendTexts: appendTexts}
		if err := templates.PendingSubmission(strconv.FormatInt(request.chatID, 10), requestID, userMessage, request.model, request.selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render pending message: %v", err)
		}
	}
}

func messageEditHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		messageID, ok := positiveID(r.PathValue("message"))
		if !ok {
			renderMessageError(w, r, http.StatusBadRequest, "A valid message is required.")
			return
		}
		if !parseMessageEditForm(w, r, messageID) {
			return
		}

		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			renderMessageEditError(w, r, http.StatusBadRequest, messageID, "Message is required.")
			return
		}
		chat := r.PathValue("chat")
		chatID, ok := positiveID(chat)
		if !ok {
			renderMessageEditError(w, r, http.StatusBadRequest, messageID, "A valid chat is required.")
			return
		}
		request, ok := parseMessageOptions(r, database, registry, chat, chatID, func(status int, message string) {
			renderMessageEditError(w, r, status, messageID, message)
		})
		if !ok {
			return
		}

		requestID, err := toolCalls.create(request.chatID)
		if err != nil {
			if errors.Is(err, errChatCompletionActive) {
				renderMessageEditError(w, r, http.StatusConflict, messageID, "A response is already in progress.")
				return
			}
			log.Printf("create edited message tool-call tracker: %v", err)
			renderMessageEditError(w, r, http.StatusInternalServerError, messageID, "Failed to prepare edited message.")
			return
		}

		err = editUserMessage(r.Context(), database, request.chatID, messageID, message, request.selected.Names(), r.Form["append"])
		if err != nil {
			toolCalls.delete(requestID)
			switch {
			case errors.Is(err, errInvalidAppendSelection):
				renderMessageEditError(w, r, http.StatusBadRequest, messageID, "Prompt append selection is invalid.")
			case errors.Is(err, errPromptAppendTooLarge):
				renderMessageEditError(w, r, http.StatusRequestEntityTooLarge, messageID, "Message with prompt appends is too large.")
			case errors.Is(err, kritui_db.ErrChatNotFound), errors.Is(err, kritui_db.ErrMessageNotFound):
				renderMessageEditError(w, r, http.StatusNotFound, messageID, "Message no longer exists.")
			case errors.Is(err, kritui_db.ErrMessageNotEditable):
				renderMessageEditError(w, r, http.StatusConflict, messageID, "Only user messages can be edited.")
			default:
				log.Printf("edit user message: %v", err)
				renderMessageEditError(w, r, http.StatusInternalServerError, messageID, "Failed to edit message.")
			}
			return
		}

		messages, err := kritui_db.GetMessages(r.Context(), database, request.chatID)
		if err != nil {
			toolCalls.delete(requestID)
			log.Printf("reload edited conversation: %v", err)
			renderMessageEditError(w, r, http.StatusInternalServerError, messageID, "Message was edited, but the conversation could not be displayed.")
			return
		}
		var fragment bytes.Buffer
		if err := templates.EditedSubmission(request.chat, requestID, messages, request.model, request.selected.Names()).Render(r.Context(), &fragment); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render edited conversation: %v", err)
			renderMessageEditError(w, r, http.StatusInternalServerError, messageID, "Message was edited, but the conversation could not be displayed.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Retarget", "#messages")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("HX-Trigger-After-Settle", "kritui:message-edited")
		_, _ = w.Write(fragment.Bytes())
	}
}

func renderCommandResult(w http.ResponseWriter, r *http.Request, result commands.Result) {
	var fragment bytes.Buffer
	if result.Body != nil {
		if err := result.Body.Render(r.Context(), &fragment); err != nil {
			log.Printf("render slash command: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Command completed, but its result could not be displayed.")
			return
		}
	}
	for name, values := range result.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if result.Body != nil && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	status := result.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if status != http.StatusNoContent && status != http.StatusNotModified {
		_, _ = w.Write(fragment.Bytes())
	}
}

func renderCommandError(w http.ResponseWriter, r *http.Request, name string, err error) {
	if errors.Is(err, commands.ErrCommandNotFound) {
		renderMessageError(w, r, http.StatusBadRequest, fmt.Sprintf("Unknown command /%s.", name))
		return
	}
	var userError *commands.UserError
	if errors.As(err, &userError) {
		status := userError.Status
		if status < 400 || status > 499 {
			status = http.StatusBadRequest
		}
		renderMessageError(w, r, status, userError.Message)
		return
	}

	log.Printf("execute slash command %q: %v", name, err)
	renderMessageError(w, r, http.StatusInternalServerError, "Failed to execute command.")
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
		if err := templates.CompletedMessage(request.chat, completedMessages...).Render(r.Context(), &fragment); err != nil {
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
	return parseMessageOptions(r, database, registry, chat, chatID, func(status int, message string) {
		renderMessageError(w, r, status, message)
	})
}

func parseMessageOptions(r *http.Request, database *sql.DB, registry *tools.Registry, chat string, chatID int64, renderError func(int, string)) (messageRequest, bool) {
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		var err error
		model, err = kritui_db.GetDefaultModel(r.Context(), database, os.Getenv("LLM_MODEL"))
		if err != nil {
			log.Printf("get default model: %v", err)
			renderError(http.StatusInternalServerError, "Failed to load settings.")
			return messageRequest{}, false
		}
	}
	selected, err := registry.Select(r.Form["tool"]...)
	if err != nil {
		renderError(http.StatusBadRequest, "Tool selection is invalid.")
		return messageRequest{}, false
	}
	return messageRequest{chat: chat, chatID: chatID, model: model, selected: selected}, true
}

// persistUserMessage resolves submitted prompt-append IDs against the current
// definitions inside the same transaction that writes the chat and inserts the
// message. Resolving in-transaction means a settings save that removes an ID
// cannot slip between validation and persistence, so stale references are never
// reintroduced into chat selections. It returns the stored message ID and the
// resolved append definitions used for rendering.
func persistUserMessage(ctx context.Context, database *sql.DB, chatID int64, message string, tools []string, appendIDs []string) (persistedUserMessage, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return persistedUserMessage{}, fmt.Errorf("begin user message transaction: %w", err)
	}
	defer tx.Rollback()

	selectedAppends, err := selectMessageAppends(ctx, tx, message, appendIDs)
	if err != nil {
		return persistedUserMessage{}, err
	}

	if err := kritui_db.UpsertChat(ctx, tx, chatID, normalizeChatTitle(message), tools, kritui_db.PromptAppendIDs(selectedAppends)); err != nil {
		return persistedUserMessage{}, err
	}

	var position int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(position) + 1, 0)
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&position); err != nil {
		return persistedUserMessage{}, fmt.Errorf("get user message position: %w", err)
	}
	id, err := kritui_db.InsertMessage(ctx, tx, chatID, position, llm.Message{
		Role:              "user",
		Content:           message,
		PromptAppendTexts: promptAppendTexts(selectedAppends),
	})
	if err != nil {
		return persistedUserMessage{}, fmt.Errorf("insert user message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return persistedUserMessage{}, fmt.Errorf("commit user message: %w", err)
	}
	return persistedUserMessage{id: id, appends: selectedAppends}, nil
}

func editUserMessage(ctx context.Context, database *sql.DB, chatID, messageID int64, message string, tools []string, appendIDs []string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin message edit transaction: %w", err)
	}
	defer tx.Rollback()

	selectedAppends, err := selectMessageAppends(ctx, tx, message, appendIDs)
	if err != nil {
		return err
	}
	if err := kritui_db.ReplaceUserMessage(ctx, tx, chatID, messageID, llm.Message{
		Role:              "user",
		Content:           message,
		PromptAppendTexts: promptAppendTexts(selectedAppends),
	}); err != nil {
		return err
	}
	if err := kritui_db.UpsertChat(ctx, tx, chatID, normalizeChatTitle(message), tools, kritui_db.PromptAppendIDs(selectedAppends)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message edit: %w", err)
	}
	return nil
}

func selectMessageAppends(ctx context.Context, database *sql.Tx, message string, appendIDs []string) ([]kritui_db.PromptAppend, error) {
	var selectedAppends []kritui_db.PromptAppend
	if len(appendIDs) > 0 {
		promptAppends, err := kritui_db.GetPromptAppends(ctx, database)
		if err != nil {
			return nil, fmt.Errorf("get prompt appends: %w", err)
		}
		selectedAppends, err = kritui_db.SelectPromptAppends(promptAppends, appendIDs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidAppendSelection, err)
		}
	}
	if oversized := appendPromptText(message, promptAppendTexts(selectedAppends)); int64(len([]byte(oversized))) > maxMessageBodyBytes {
		return nil, errPromptAppendTooLarge
	}
	return selectedAppends, nil
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

func parseMessageEditForm(w http.ResponseWriter, r *http.Request, messageID int64) bool {
	if err := parseLimitedForm(w, r, maxMessageBodyBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			renderMessageEditError(w, r, http.StatusRequestEntityTooLarge, messageID, "Request body is too large.")
			return false
		}
		renderMessageEditError(w, r, http.StatusBadRequest, messageID, "Invalid message form.")
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

func renderMessageEditError(w http.ResponseWriter, r *http.Request, status int, messageID int64, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.MessageEditError(messageID, message).Render(r.Context(), w); err != nil {
		log.Printf("render message edit error: %v", err)
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
