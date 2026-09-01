package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
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
	toolCallUnclaimedTrackerTTL = 30 * time.Second
	toolCallFinishedTrackerTTL  = 5 * time.Minute
	toolCallHeartbeatInterval   = 30 * time.Second
	toolCallCloseEvent          = "close"
)

var errChatCompletionActive = errors.New("chat completion already active")

type toolCallTracker struct {
	mu        sync.RWMutex
	calls     []llm.ToolCall
	running   string
	errors    map[string]string
	terminal  string
	finished  bool
	updates   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newToolCallTracker() *toolCallTracker {
	return &toolCallTracker{
		updates: make(chan struct{}),
		done:    make(chan struct{}),
		errors:  make(map[string]string),
	}
}

func (t *toolCallTracker) observe(call llm.ToolCall, running bool, result string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if running {
		t.calls = append(t.calls, call)
		t.running = call.ID
	} else if t.running == call.ID {
		t.running = ""
		if message := llm.ToolErrorMessage(result); message != "" {
			t.errors[call.ID] = message
		}
	}
	close(t.updates)
	t.updates = make(chan struct{})
}

func (t *toolCallTracker) streamSnapshot() ([]llm.ToolCall, string, map[string]string, string, bool, <-chan struct{}) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	errors := make(map[string]string, len(t.errors))
	for id, message := range t.errors {
		errors[id] = message
	}
	return append([]llm.ToolCall(nil), t.calls...), t.running, errors, t.terminal, t.finished, t.updates
}

func (t *toolCallTracker) finish(content string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.terminal = content
	t.finished = true
	close(t.updates)
	t.updates = make(chan struct{})
}

// isFinished reports whether the tracker reached its terminal state. Live
// completion exclusivity per chat derives from this predicate.
func (t *toolCallTracker) isFinished() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.finished
}

func (t *toolCallTracker) close() {
	t.closeOnce.Do(func() {
		close(t.done)
	})
}

type toolCallTrackerEntry struct {
	tracker *toolCallTracker
	chatID  int64
	model   string
	tools   []string
	started bool
	expiry  *time.Timer
}

type activeCompletion struct {
	requestID string
	model     string
	tools     []string
	started   bool
}

// toolCallStore tracks completion requests by opaque request ID. At most one
// live (unclaimed or running, not yet finished) tracker may exist per chat;
// finished trackers are retained for reconnect replay until their TTL lapses.
type toolCallStore struct {
	mu           sync.RWMutex
	entries      map[string]*toolCallTrackerEntry
	unclaimedTTL time.Duration
	finishedTTL  time.Duration
}

func newToolCallStore() *toolCallStore {
	return &toolCallStore{
		entries:      make(map[string]*toolCallTrackerEntry),
		unclaimedTTL: toolCallUnclaimedTrackerTTL,
		finishedTTL:  toolCallFinishedTrackerTTL,
	}
}

// liveEntryFor returns the unfinished entry blocking a new completion for the
// chat, if any. Finished entries retained for replay never block.
func (s *toolCallStore) liveEntryFor(chatID int64) (string, *toolCallTrackerEntry) {
	for id, entry := range s.entries {
		if entry.chatID == chatID && !entry.tracker.isFinished() {
			return id, entry
		}
	}
	return "", nil
}

func (s *toolCallStore) create(chatID int64, model string, tools []string) (string, error) {
	s.mu.Lock()
	if _, existing := s.liveEntryFor(chatID); existing != nil {
		s.mu.Unlock()
		return "", errChatCompletionActive
	}

	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		s.mu.Unlock()
		return "", err
	}
	id := hex.EncodeToString(token[:])

	entry := &toolCallTrackerEntry{
		tracker: newToolCallTracker(),
		chatID:  chatID,
		model:   model,
		tools:   append([]string(nil), tools...),
	}
	s.entries[id] = entry
	entry.expiry = time.AfterFunc(s.unclaimedTTL, func() {
		s.expireUnclaimed(id)
	})
	s.mu.Unlock()
	return id, nil
}

func (s *toolCallStore) claim(id string, chatID int64, model string, tools []string) (*toolCallTracker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok || entry.started || entry.chatID != chatID {
		return nil, false
	}
	entry.started = true
	entry.model = model
	entry.tools = append([]string(nil), tools...)
	entry.expiry.Stop()
	entry.expiry = nil
	return entry.tracker, true
}

func (s *toolCallStore) active(chatID int64) (activeCompletion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, entry := s.liveEntryFor(chatID)
	if entry == nil {
		return activeCompletion{}, false
	}
	return activeCompletion{
		requestID: id,
		model:     entry.model,
		tools:     append([]string(nil), entry.tools...),
		started:   entry.started,
	}, true
}

func (s *toolCallStore) get(id string) (*toolCallTracker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	return entry.tracker, true
}

func (s *toolCallStore) delete(id string) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	delete(s.entries, id)
	if ok && entry.expiry != nil {
		entry.expiry.Stop()
	}
	s.mu.Unlock()

	if ok {
		entry.tracker.close()
	}
}

func (s *toolCallStore) finish(id string, content string) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	entry.tracker.finish(content)
	if entry.expiry != nil {
		entry.expiry.Stop()
	}
	entry.expiry = time.AfterFunc(s.finishedTTL, func() {
		s.delete(id)
	})
	s.mu.Unlock()
}

func (s *toolCallStore) expireUnclaimed(id string) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	if !ok || entry.started {
		s.mu.Unlock()
		return
	}
	delete(s.entries, id)
	s.mu.Unlock()

	entry.tracker.close()
}

func messageToolStreamHandler(toolCalls *toolCallStore) http.HandlerFunc {
	return messageToolStreamHandlerWithHeartbeat(toolCalls, toolCallHeartbeatInterval)
}

func messageToolStreamHandlerWithHeartbeat(toolCalls *toolCallStore, heartbeatInterval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		requestID := r.URL.Query().Get("request")
		tracker, ok := toolCalls.get(requestID)
		if !ok {
			if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
				flusher.Flush()
			}
			return
		}

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()

		for {
			select {
			case <-tracker.done:
				if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
					flusher.Flush()
				}
				return
			default:
			}

			calls, running, toolErrors, terminal, finished, updates := tracker.streamSnapshot()
			if finished {
				if err := writeServerSentPartial(w, "#completion-"+requestID, "outerHTML", terminal); err == nil {
					flusher.Flush()
				}
				return
			}
			var content strings.Builder
			if err := templates.ToolCalls(calls, running, toolErrors).Render(r.Context(), &content); err != nil {
				log.Printf("render tool-call stream: %v", err)
				return
			}
			if err := writeServerSentPartial(w, "#completion-tools-"+requestID, "innerHTML", content.String()); err != nil {
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
			case <-heartbeat.C:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

func writeServerSentEvent(w io.Writer, event string, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
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

func writeServerSentPartial(w io.Writer, target, swap, content string) error {
	partial := fmt.Sprintf(`<hx-partial hx-target="%s" hx-swap="%s">%s</hx-partial>`, html.EscapeString(target), html.EscapeString(swap), content)
	return writeServerSentEvent(w, "", partial)
}
