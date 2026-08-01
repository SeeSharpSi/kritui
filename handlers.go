package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/tools"
)

const (
	toolCallTrackerTTL          = 10 * time.Minute
	toolCallUnclaimedTrackerTTL = 30 * time.Second
)

var errChatCompletionActive = errors.New("chat completion already active")

const (
	toolCallUpdateEvent = "tools"
	toolCallCloseEvent  = "close"
)

type toolCallTracker struct {
	mu        sync.RWMutex
	calls     []llm.ToolCall
	running   string
	updates   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newToolCallTracker() *toolCallTracker {
	return &toolCallTracker{
		updates: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (t *toolCallTracker) observe(call llm.ToolCall, running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if running {
		t.calls = append(t.calls, call)
		t.running = call.ID
	} else if t.running == call.ID {
		t.running = ""
	}
	close(t.updates)
	t.updates = make(chan struct{})
}

func (t *toolCallTracker) snapshot() ([]llm.ToolCall, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return append([]llm.ToolCall(nil), t.calls...), t.running
}

func (t *toolCallTracker) streamSnapshot() ([]llm.ToolCall, string, <-chan struct{}) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return append([]llm.ToolCall(nil), t.calls...), t.running, t.updates
}

func (t *toolCallTracker) close() {
	t.closeOnce.Do(func() {
		close(t.done)
	})
}

type toolCallTrackerEntry struct {
	tracker *toolCallTracker
	chatID  int64
	created time.Time
	started bool
}

type toolCallStore struct {
	mu          sync.RWMutex
	trackers    map[string]*toolCallTrackerEntry
	activeChats map[int64]string
}

func newToolCallStore() *toolCallStore {
	return &toolCallStore{
		trackers:    make(map[string]*toolCallTrackerEntry),
		activeChats: make(map[int64]string),
	}
}

func (s *toolCallStore) create(chatID int64) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(token[:])
	now := time.Now()

	var expired []*toolCallTracker
	s.mu.Lock()
	for existingID, entry := range s.trackers {
		ttl := toolCallTrackerTTL
		if !entry.started {
			ttl = toolCallUnclaimedTrackerTTL
		}
		if now.Sub(entry.created) >= ttl {
			delete(s.trackers, existingID)
			if s.activeChats[entry.chatID] == existingID {
				delete(s.activeChats, entry.chatID)
			}
			expired = append(expired, entry.tracker)
		}
	}
	if _, active := s.activeChats[chatID]; active {
		s.mu.Unlock()
		for _, tracker := range expired {
			tracker.close()
		}
		return "", errChatCompletionActive
	}
	s.trackers[id] = &toolCallTrackerEntry{
		tracker: newToolCallTracker(),
		chatID:  chatID,
		created: now,
	}
	s.activeChats[chatID] = id
	s.mu.Unlock()

	for _, tracker := range expired {
		tracker.close()
	}
	return id, nil
}

func (s *toolCallStore) claim(id string, chatID int64) (*toolCallTracker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.trackers[id]
	if !ok || entry.started || entry.chatID != chatID {
		return nil, false
	}
	entry.started = true
	return entry.tracker, true
}

func (s *toolCallStore) get(id string) (*toolCallTracker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.trackers[id]
	if !ok {
		return nil, false
	}
	return entry.tracker, true
}

func (s *toolCallStore) delete(id string) {
	s.mu.Lock()
	entry, ok := s.trackers[id]
	delete(s.trackers, id)
	if ok && s.activeChats[entry.chatID] == id {
		delete(s.activeChats, entry.chatID)
	}
	s.mu.Unlock()

	if ok {
		entry.tracker.close()
	}
}

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
		enabledTools, err := kritui_db.GetChatTools(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get chat tools: %v", err)
			http.Error(w, "failed to get chat tools", http.StatusInternalServerError)
			return
		}
		models, selectedModel := availableModels(r)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home(chat, messages, models, selectedModel, registry.Names(), enabledTools).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
	}
}

func historyHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		chatID, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || chatID <= 0 {
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

		chats, err := kritui_db.GetChats(r.Context(), database)
		if err != nil {
			log.Printf("get chats: %v", err)
			http.Error(w, "failed to get chats", http.StatusInternalServerError)
			return
		}

		if err := templates.HistoryList(chat, chats).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render chat history", http.StatusInternalServerError)
		}
	}
}

func deleteChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, err := strconv.ParseInt(r.PathValue("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		current := r.URL.Query().Get("current")
		currentChatID, err := strconv.ParseInt(current, 10, 64)
		if err != nil || currentChatID <= 0 {
			http.Error(w, "valid current chat is required", http.StatusBadRequest)
			return
		}

		if _, err := database.ExecContext(r.Context(), `DELETE FROM chats WHERE id = ?`, chatID); err != nil {
			log.Printf("delete chat: %v", err)
			http.Error(w, "failed to delete chat", http.StatusInternalServerError)
			return
		}
		if chatID == currentChatID {
			w.Header().Set("HX-Redirect", "/")
			return
		}

		chats, err := kritui_db.GetChats(r.Context(), database)
		if err != nil {
			log.Printf("get chats: %v", err)
			http.Error(w, "failed to get chats", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.HistoryList(current, chats).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render chat history", http.StatusInternalServerError)
		}
	}
}

func renameChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, err := strconv.ParseInt(r.PathValue("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		current := r.URL.Query().Get("current")
		currentChatID, err := strconv.ParseInt(current, 10, 64)
		if err != nil || currentChatID <= 0 {
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

		chats, err := kritui_db.GetChats(r.Context(), database)
		if err != nil {
			log.Printf("get chats: %v", err)
			http.Error(w, "failed to get chats", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.HistoryList(current, chats).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render chat history", http.StatusInternalServerError)
		}
	}
}

func messageHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			renderMessageError(w, r, http.StatusBadRequest, "Message is required.")
			return
		}

		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		model := strings.TrimSpace(r.FormValue("model"))
		if model == "" {
			model = os.Getenv("LLM_MODEL")
		}
		selected, err := registry.Select(r.Form["tool"]...)
		if err != nil {
			renderMessageError(w, r, http.StatusBadRequest, "Tool selection is invalid.")
			return
		}
		requestID, err := toolCalls.create(chatID)
		if err != nil {
			if errors.Is(err, errChatCompletionActive) {
				renderMessageError(w, r, http.StatusConflict, "A response is already in progress.")
				return
			}
			log.Printf("create tool-call tracker: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to prepare message.")
			return
		}
		if err := persistUserMessage(r.Context(), database, chatID, message, selected.Names()); err != nil {
			toolCalls.delete(requestID)
			log.Printf("store user message: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to store message.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{Role: "user", Content: message}
		if err := templates.PendingSubmission(strconv.FormatInt(chatID, 10), requestID, userMessage, model, selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render pending message: %v", err)
		}
	}
}

func messageCompletionHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore, toolCallLogger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		chatID, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || chatID <= 0 {
			renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		model := strings.TrimSpace(r.FormValue("model"))
		if model == "" {
			model = os.Getenv("LLM_MODEL")
		}
		selected, err := registry.Select(r.Form["tool"]...)
		if err != nil {
			renderMessageError(w, r, http.StatusBadRequest, "Tool selection is invalid.")
			return
		}

		requestID := strings.TrimSpace(r.FormValue("request"))
		tracker, ok := toolCalls.claim(requestID, chatID)
		if !ok {
			renderMessageError(w, r, http.StatusBadRequest, "This completion request is no longer valid.")
			return
		}
		defer toolCalls.delete(requestID)

		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to load conversation.", model, selected.Names())
			return
		}
		if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}
		client, err := llm.New(os.Getenv("LLM_KEY"), model, os.Getenv("LLM_ENDPOINT"))
		if err != nil {
			log.Printf("configure llm: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to configure model.", model, selected.Names())
			return
		}

		position := len(messages)
		conversation, err := llm.NewConversation(client, selected, messages...)
		if err != nil {
			log.Printf("configure conversation: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to configure conversation.", model, selected.Names())
			return
		}
		conversation.SetToolCallLogger(toolCallLogger)
		conversation.SetToolCallObserver(tracker.observe)
		completion, err := conversation.Complete(r.Context())
		if err != nil {
			log.Printf("complete message: %v", err)
			renderCompletionError(w, r, http.StatusBadGateway, chat, "Failed to complete message.", model, selected.Names())
			return
		}
		completedMessages := conversation.Messages()[len(messages)+1:]
		tx, err := database.BeginTx(r.Context(), nil)
		if err != nil {
			log.Printf("begin message transaction: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to store response.", model, selected.Names())
			return
		}
		for index, completedMessage := range completedMessages {
			if _, err := kritui_db.InsertMessage(r.Context(), tx, chatID, position+index, completedMessage); err != nil {
				_ = tx.Rollback()
				log.Printf("store message: %v", err)
				renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to store response.", model, selected.Names())
				return
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("commit messages: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to store response.", model, selected.Names())
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
		chat := r.URL.Query().Get("chat")
		chatID, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || chatID <= 0 {
			renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		model := strings.TrimSpace(r.FormValue("model"))
		if model == "" {
			model = os.Getenv("LLM_MODEL")
		}
		selected, err := registry.Select(r.Form["tool"]...)
		if err != nil {
			renderMessageError(w, r, http.StatusBadRequest, "Tool selection is invalid.")
			return
		}

		var role string
		err = database.QueryRowContext(r.Context(), `
			SELECT role
			FROM messages
			WHERE chat_id = ?
			ORDER BY position DESC
			LIMIT 1
		`, chatID).Scan(&role)
		if err == sql.ErrNoRows {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}
		if err != nil {
			log.Printf("get retry message: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to prepare retry.", model, selected.Names())
			return
		}
		if role != "user" {
			renderMessageError(w, r, http.StatusConflict, "No message is waiting for completion.")
			return
		}

		requestID, err := toolCalls.create(chatID)
		if err != nil {
			if errors.Is(err, errChatCompletionActive) {
				renderCompletionError(w, r, http.StatusConflict, chat, "A response is already in progress.", model, selected.Names())
				return
			}
			log.Printf("create retry tracker: %v", err)
			renderCompletionError(w, r, http.StatusInternalServerError, chat, "Failed to prepare retry.", model, selected.Names())
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.PendingCompletion(chat, requestID, model, selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			log.Printf("render retry: %v", err)
		}
	}
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

func renderCompletionError(w http.ResponseWriter, r *http.Request, status int, chatID string, message string, model string, tools []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.CompletionError(chatID, message, model, tools).Render(r.Context(), w); err != nil {
		log.Printf("render completion error: %v", err)
	}
}

func messageToolStreamHandler(toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracker, ok := toolCalls.get(r.URL.Query().Get("request"))
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		for {
			select {
			case <-tracker.done:
				if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
					flusher.Flush()
				}
				return
			default:
			}

			calls, running, updates := tracker.streamSnapshot()
			var content strings.Builder
			if err := templates.ToolCalls(calls, running).Render(r.Context(), &content); err != nil {
				log.Printf("render tool-call stream: %v", err)
				return
			}
			if err := writeServerSentEvent(w, toolCallUpdateEvent, content.String()); err != nil {
				return
			}
			flusher.Flush()

			select {
			case <-updates:
			case <-tracker.done:
				if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
					flusher.Flush()
				}
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}

func writeServerSentEvent(w io.Writer, event string, data string) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
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
