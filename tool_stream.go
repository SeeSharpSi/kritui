package main

import (
	"context"
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

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
)

const (
	toolCallUnclaimedTrackerTTL = 30 * time.Second
	toolCallFinishedTrackerTTL  = 5 * time.Minute
	toolCallHeartbeatInterval   = 30 * time.Second
	toolCallCloseEvent          = "close"
)

var (
	errChatCompletionActive = errors.New("chat completion already active")
	errInvalidToolCallOwner = errors.New("invalid tool-call owner")
)

// legacyToolCallOwner is the reserved owner account used by direct handler
// tests and maintenance helpers that call the owner-agnostic store wrappers.
// Production request handlers always pass the authenticated database user.
const legacyToolCallOwner int64 = kritui_db.LegacyOwnerID

type toolCallTracker struct {
	mu           sync.RWMutex
	calls        []llm.ToolCall
	running      string
	errors       map[string]string
	terminal     string
	finished     bool
	presentation string
	revision     uint64
	updates      chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
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
	state := t.snapshot()
	return state.calls, state.running, state.errors, state.terminal, state.finished, state.updates
}

type toolCallSnapshot struct {
	calls        []llm.ToolCall
	running      string
	errors       map[string]string
	terminal     string
	finished     bool
	presentation string
	revision     uint64
	updates      <-chan struct{}
}

func (t *toolCallTracker) snapshot() toolCallSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	errors := make(map[string]string, len(t.errors))
	for id, message := range t.errors {
		errors[id] = message
	}
	return toolCallSnapshot{
		calls: append([]llm.ToolCall(nil), t.calls...), running: t.running, errors: errors,
		terminal: t.terminal, finished: t.finished, presentation: t.presentation,
		revision: t.revision, updates: t.updates,
	}
}

// present refreshes the transcript and inbox without changing in-flight tools.
func (t *toolCallTracker) present(content string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.presentLocked(content)
}

func (t *toolCallTracker) presentLocked(content string) {
	t.presentation = content
	t.revision++
	close(t.updates)
	t.updates = make(chan struct{})
}

// presentCheckpoint atomically moves archived batches into the transcript and
// keeps the entire unfinished tool chain in the expanded live list. Only the
// completion worker calls this, after all tools in a batch have finished.
func (t *toolCallTracker) presentCheckpoint(ctx context.Context, chat string, messages []llm.Message, items []kritui_db.InboxItem) {
	view := templates.ActiveCompletionView(messages)
	content := renderCompletionFragment(ctx, templates.CompletionResult(chat, view.Messages, items))
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = view.ToolCalls
	t.running = ""
	t.errors = view.ToolErrors
	if t.errors == nil {
		t.errors = make(map[string]string)
	}
	t.presentLocked(content)
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
	owner   int64
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
	// workMu serializes durable admission and completion boundaries. It is never
	// held while calling a provider or executing a tool.
	workMu       sync.Mutex
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

// liveEntryFor returns the unfinished entry for owner's chat, if any. Finished
// entries retained for replay never block. Owner is part of every lookup so a
// request ID from one account can never observe another account's tracker.
func (s *toolCallStore) liveEntryFor(owner, chatID int64) (string, *toolCallTrackerEntry) {
	for id, entry := range s.entries {
		if entry.owner == owner && entry.chatID == chatID && !entry.tracker.isFinished() {
			return id, entry
		}
	}
	return "", nil
}

// create is the trusted-owner wrapper retained for direct tests and
// maintenance helpers. Production handlers use createForUser.
func (s *toolCallStore) create(chatID int64, model string, tools []string) (string, error) {
	return s.createForUser(legacyToolCallOwner, chatID, model, tools)
}

func (s *toolCallStore) createForUser(owner, chatID int64, model string, tools []string) (string, error) {
	if owner <= 0 {
		return "", errInvalidToolCallOwner
	}

	s.mu.Lock()
	if _, existing := s.liveEntryFor(owner, chatID); existing != nil {
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
		owner:   owner,
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

// claim is the trusted-owner wrapper retained for direct tests and maintenance
// helpers. Production handlers use claimForUser.
func (s *toolCallStore) claim(id string, chatID int64, model string, tools []string) (*toolCallTracker, bool) {
	return s.claimForUser(legacyToolCallOwner, id, chatID, model, tools)
}

func (s *toolCallStore) claimForUser(owner int64, id string, chatID int64, model string, tools []string) (*toolCallTracker, bool) {
	if owner <= 0 {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok || entry.owner != owner || entry.started || entry.chatID != chatID {
		return nil, false
	}
	entry.started = true
	entry.model = model
	entry.tools = append([]string(nil), tools...)
	entry.expiry.Stop()
	entry.expiry = nil
	return entry.tracker, true
}

// active is the trusted-owner wrapper retained for direct tests and
// maintenance helpers. Production handlers use activeForUser.
func (s *toolCallStore) active(chatID int64) (activeCompletion, bool) {
	return s.activeForUser(legacyToolCallOwner, chatID)
}

func (s *toolCallStore) activeForUser(owner, chatID int64) (activeCompletion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, entry := s.liveEntryFor(owner, chatID)
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

// get is the trusted-owner wrapper retained for direct tests and maintenance
// helpers. Production handlers use getForUser.
func (s *toolCallStore) get(id string) (*toolCallTracker, bool) {
	return s.getForUser(legacyToolCallOwner, id)
}

func (s *toolCallStore) getForUser(owner int64, id string) (*toolCallTracker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.entries[id]
	if !ok || entry.owner != owner {
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
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		requestID := r.URL.Query().Get("request")
		tracker, ok := toolCalls.getForUser(kritui_db.UserID(r.Context()), requestID)
		if !ok {
			if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
				flusher.Flush()
			}
			return
		}

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()
		var presentationRevision uint64

		for {
			select {
			case <-tracker.done:
				if err := writeServerSentEvent(w, toolCallCloseEvent, ""); err == nil {
					flusher.Flush()
				}
				return
			default:
			}

			state := tracker.snapshot()
			if state.finished {
				if err := writeServerSentPartial(w, "#completion-"+requestID, "outerHTML", state.terminal); err == nil {
					flusher.Flush()
				}
				return
			}
			if state.revision != presentationRevision {
				if err := writeServerSentEvent(w, "", state.presentation); err != nil {
					return
				}
				presentationRevision = state.revision
			}
			var content strings.Builder
			if err := templates.ToolCalls(state.calls, state.running, state.errors).Render(r.Context(), &content); err != nil {
				log.Printf("render tool-call stream: %v", err)
				return
			}
			if err := writeServerSentPartial(w, "#completion-tools-"+requestID, "innerHTML", content.String()); err != nil {
				return
			}
			flusher.Flush()

			select {
			case <-state.updates:
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
