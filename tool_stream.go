package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
)

const (
	toolCallTrackerTTL          = 10 * time.Minute
	toolCallUnclaimedTrackerTTL = 30 * time.Second
	toolCallUpdateEvent         = "tools"
	toolCallCloseEvent          = "close"
)

var errChatCompletionActive = errors.New("chat completion already active")

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
	_, active := s.activeChats[chatID]
	if !active {
		s.trackers[id] = &toolCallTrackerEntry{
			tracker: newToolCallTracker(),
			chatID:  chatID,
			created: now,
		}
		s.activeChats[chatID] = id
	}
	s.mu.Unlock()

	for _, tracker := range expired {
		tracker.close()
	}
	if active {
		return "", errChatCompletionActive
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
