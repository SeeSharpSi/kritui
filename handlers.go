package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

const toolCallTrackerTTL = 10 * time.Minute

type toolCallTracker struct {
	mu      sync.RWMutex
	calls   []llm.ToolCall
	running string
}

func (t *toolCallTracker) observe(call llm.ToolCall, running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if running {
		t.calls = append(t.calls, call)
		t.running = call.ID
		return
	}
	if t.running == call.ID {
		t.running = ""
	}
}

func (t *toolCallTracker) snapshot() ([]llm.ToolCall, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return append([]llm.ToolCall(nil), t.calls...), t.running
}

type toolCallTrackerEntry struct {
	tracker *toolCallTracker
	created time.Time
	started bool
}

type toolCallStore struct {
	mu       sync.RWMutex
	trackers map[string]*toolCallTrackerEntry
}

func newToolCallStore() *toolCallStore {
	return &toolCallStore{trackers: make(map[string]*toolCallTrackerEntry)}
}

func (s *toolCallStore) create() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(token[:])
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for existingID, entry := range s.trackers {
		if now.Sub(entry.created) >= toolCallTrackerTTL {
			delete(s.trackers, existingID)
		}
	}
	s.trackers[id] = &toolCallTrackerEntry{
		tracker: &toolCallTracker{},
		created: now,
	}
	return id, nil
}

func (s *toolCallStore) claim(id string) (*toolCallTracker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.trackers[id]
	if !ok || entry.started {
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
	defer s.mu.Unlock()
	delete(s.trackers, id)
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
		models, selectedModel := availableModels(r)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Home(chat, messages, models, selectedModel, registry.Names()).Render(r.Context(), w); err != nil {
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

		chats, err := kritui_db.GetChats(r.Context(), database)
		if err != nil {
			log.Printf("get chats: %v", err)
			http.Error(w, "failed to get chats", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
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

func chatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
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

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ChatMessageList(chat, messages).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render chat", http.StatusInternalServerError)
		}
	}
}

func messageHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore) http.HandlerFunc {
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
		selected, err := registry.Select(r.Form["tool"]...)
		if err != nil {
			http.Error(w, "invalid tool selection", http.StatusBadRequest)
			return
		}
		requestID, err := toolCalls.create()
		if err != nil {
			log.Printf("create tool-call tracker: %v", err)
			http.Error(w, "failed to prepare message", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		userMessage := llm.Message{Role: "user", Content: message}
		if err := templates.PendingSubmission(strconv.FormatInt(chatID, 10), requestID, userMessage, model, selected.Names()).Render(r.Context(), w); err != nil {
			toolCalls.delete(requestID)
			http.Error(w, "failed to render pending message", http.StatusInternalServerError)
		}
	}
}

func messageCompletionHandler(database *sql.DB, registry *tools.Registry, toolCalls *toolCallStore, toolCallLogger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message := strings.TrimSpace(r.FormValue("message"))
		if message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}
		requestID := strings.TrimSpace(r.FormValue("request"))
		tracker, ok := toolCalls.claim(requestID)
		if !ok {
			http.Error(w, "valid request is required", http.StatusBadRequest)
			return
		}
		defer toolCalls.delete(requestID)

		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
		if err != nil || chatID <= 0 {
			http.Error(w, "valid chat is required", http.StatusBadRequest)
			return
		}
		selected, err := registry.Select(r.Form["tool"]...)
		if err != nil {
			http.Error(w, "invalid tool selection", http.StatusBadRequest)
			return
		}
		if _, err := database.ExecContext(r.Context(), `
			INSERT INTO chats (id, title) VALUES (?, ?)
			ON CONFLICT (id) DO UPDATE SET title = excluded.title WHERE chats.title = ''
		`, chatID, message); err != nil {
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

		position := len(messages)
		conversation, err := llm.NewConversation(client, selected, messages...)
		if err != nil {
			log.Printf("configure conversation: %v", err)
			http.Error(w, "failed to configure conversation", http.StatusInternalServerError)
			return
		}
		conversation.SetToolCallLogger(toolCallLogger)
		conversation.SetToolCallObserver(tracker.observe)
		completion, err := conversation.Send(r.Context(), message)
		if err != nil {
			log.Printf("complete message: %v", err)
			http.Error(w, "failed to complete message", http.StatusBadGateway)
			return
		}
		completedMessages := conversation.Messages()[len(messages)+1:]
		for index, completedMessage := range completedMessages {
			if _, err := kritui_db.InsertMessage(r.Context(), database, chatID, position+index, completedMessage); err != nil {
				log.Printf("store message: %v", err)
				http.Error(w, "failed to store message", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		calls, _ := tracker.snapshot()
		if err := templates.CompletedMessage(calls, completion.Message).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render message", http.StatusInternalServerError)
		}
	}
}

func messageToolStatusHandler(toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracker, ok := toolCalls.get(r.URL.Query().Get("request"))
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		calls, running := tracker.snapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ToolCalls(calls, running).Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render tool calls", http.StatusInternalServerError)
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
