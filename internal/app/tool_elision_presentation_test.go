package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/tools"
)

// toolElisionBulkTool returns a result large enough for the token-budget mode
// to replace with its marker.
type toolElisionBulkTool struct{}

func (toolElisionBulkTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "elision_bulk",
		Description: "Returns a large deterministic result",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`),
	}
}

func (toolElisionBulkTool) Execute(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("detailed tool result ", 40), nil
}

func newToolElisionBulkRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry, err := tools.NewRegistry(toolElisionBulkTool{})
	if err != nil {
		t.Fatalf("create bulk elision registry: %v", err)
	}
	return registry
}

func requireToolCallElided(t *testing.T, markup, callID string, want bool) {
	t.Helper()
	marker := `data-tool-call-id="` + callID + `"`
	index := strings.Index(markup, marker)
	if index < 0 {
		t.Fatalf("markup has no tool call %q: %s", callID, markup)
	}
	start := strings.LastIndex(markup[:index], `<div class="tool-call`)
	if start < 0 {
		t.Fatalf("tool call %q has no opening tag: %s", callID, markup)
	}
	got := strings.Contains(markup[start:index], "tool-elided")
	if got != want {
		t.Fatalf("tool call %q elided = %v, want %v; tag: %s", callID, got, want, markup[start:index])
	}
}

func snapshotToolCall(t *testing.T, database *sql.DB, chatID int64, callID string) (found bool, messageID int64, callIndex int, call llm.ToolCall, result llm.Message) {
	t.Helper()
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	for _, message := range snapshot.Messages {
		if message.Role == "assistant" {
			for index, candidate := range message.ToolCalls {
				if candidate.ID == callID {
					for _, toolResult := range snapshot.Messages {
						if toolResult.Role == "tool" && toolResult.ToolCallID == callID {
							return true, message.ID, index, candidate, toolResult
						}
					}
					t.Fatalf("tool call %q has no stored result", callID)
				}
			}
		}
	}
	t.Fatalf("snapshot has no tool call %q", callID)
	return false, 0, 0, llm.ToolCall{}, llm.Message{}
}

func requireStoredToolCallElision(t *testing.T, database *sql.DB, chatID int64, callID string, want bool) {
	t.Helper()
	found, _, _, call, _ := snapshotToolCall(t, database, chatID, callID)
	if !found {
		t.Fatalf("tool call %q is missing", callID)
	}
	if call.ResultElided != want {
		t.Fatalf("stored tool call %q elided = %v, want %v", callID, call.ResultElided, want)
	}
}

func TestToolResultElisionUpdatesArchivedNamesBeforeCompletionFinishes(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const (
		chatID = 84
		model  = "elision-presentation-model"
	)
	oldResultID, oldResult := seedToolElisionHistory(t, database, chatID)
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 10}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: model, MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	gates := []*inboxTestGate{newInboxTestGate(), newInboxTestGate()}
	defer func() {
		for _, gate := range gates {
			gate.releaseNow()
		}
	}()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index >= len(gates) {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gates[index].wait(r)
		if index == 0 {
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "call-new", Name: "lookup", Key: "new"})
			return
		}
		inboxTestWriteFinal(w, "Fresh answer")
	})
	inboxTestUseProvider(t, provider, model)

	registry := newToolElisionLookupRegistry(t)
	store := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"New question"}, "model": {model}, "tool": {"lookup"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "lookup")

	provider.waitForRequest(t, 1)
	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	requireToolCallElided(t, state.presentation, "old-call", true)
	requireStoredToolCallElision(t, database, chatID, "old-call", true)
	_, _, _, _, oldStored := snapshotToolCall(t, database, chatID, "old-call")
	if oldStored.ID != oldResultID || oldStored.Content != oldResult {
		t.Fatalf("archived result body changed: got ID %d content %q", oldStored.ID, oldStored.Content)
	}

	gates[0].releaseNow()
	provider.waitForRequest(t, 2)
	state = tracker.snapshot()
	requireToolCallElided(t, state.presentation, "old-call", true)
	if len(state.calls) != 1 || state.calls[0].ID != "call-new" || state.calls[0].ResultElided {
		t.Fatalf("live calls after checkpoint = %#v, want full call-new", state.calls)
	}
	requireStoredToolCallElision(t, database, chatID, "old-call", true)
	requireStoredToolCallElision(t, database, chatID, "call-new", false)
	if _, _, _, _, fresh := snapshotToolCall(t, database, chatID, "call-new"); fresh.Content != "found value" {
		t.Fatalf("fresh result body = %q, want original result", fresh.Content)
	}

	gates[1].releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Fresh answer")
	requireToolCallElided(t, terminal, "old-call", true)
	requireToolCallElided(t, terminal, "call-new", false)
}

func TestToolResultElisionUpdatesLiveSameTurnNames(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const model = "live-elision-model"
	chatID := seedChat(t, database, "Live elision", []string{"elision_bulk"}, nil)
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 10}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: model, MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	gates := []*inboxTestGate{newInboxTestGate(), newInboxTestGate(), newInboxTestGate()}
	defer func() {
		for _, gate := range gates {
			gate.releaseNow()
		}
	}()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index >= len(gates) {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gates[index].wait(r)
		switch index {
		case 0:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "bulk-one", Name: "elision_bulk", Key: "one"})
		case 1:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "bulk-two", Name: "elision_bulk", Key: "two"})
		default:
			inboxTestWriteFinal(w, "Live answer")
		}
	})
	inboxTestUseProvider(t, provider, model)

	registry := newToolElisionBulkRegistry(t)
	store := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"Ask with tools"}, "model": {model}, "tool": {"elision_bulk"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "elision_bulk")

	provider.waitForRequest(t, 1)
	gates[0].releaseNow()
	provider.waitForRequest(t, 2)
	gates[1].releaseNow()
	provider.waitForRequest(t, 3)

	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	if len(state.calls) != 2 {
		t.Fatalf("live calls = %#v, want two", state.calls)
	}
	if state.calls[0].ID != "bulk-one" || !state.calls[0].ResultElided {
		t.Fatalf("live call bulk-one = %#v, want elided", state.calls[0])
	}
	if state.calls[1].ID != "bulk-two" || state.calls[1].ResultElided {
		t.Fatalf("live call bulk-two = %#v, want full", state.calls[1])
	}
	requireStoredToolCallElision(t, database, chatID, "bulk-one", true)
	requireStoredToolCallElision(t, database, chatID, "bulk-two", false)

	gates[2].releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Live answer")
	requireToolCallElided(t, terminal, "bulk-one", true)
	requireToolCallElided(t, terminal, "bulk-two", false)
	for _, callID := range []string{"bulk-one", "bulk-two"} {
		if _, _, _, _, result := snapshotToolCall(t, database, chatID, callID); result.Content != strings.Repeat("detailed tool result ", 40) {
			t.Fatalf("stored result body for %q changed: %q", callID, result.Content)
		}
	}
}

func TestToolResultElisionKeepsErroredResultBody(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const model = "error-elision-model"
	chatID := seedChat(t, database, "Error elision", []string{"lookup"}, nil)
	ctx := context.Background()
	errorResult := "Tool error: archive unavailable " + strings.Repeat("detail ", 40)
	messages := []llm.Message{
		{Role: "user", Content: "Old question"},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID: "error-call", Type: "function",
				Function: llm.FunctionCall{Name: "lookup", Arguments: "{}"},
			}},
		},
		{Role: "tool", Content: errorResult, ToolCallID: "error-call"},
		{Role: "assistant", Content: "Old answer"},
	}
	for position, message := range messages {
		if _, err := kritui_db.InsertMessage(ctx, database, chatID, position, message); err != nil {
			t.Fatalf("insert history message %d: %v", position, err)
		}
	}
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 10}
	if err := kritui_db.SaveSettings(ctx, database, kritui_db.SettingsUpdate{
		Model: model, MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	gate := newInboxTestGate()
	defer gate.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index != 0 {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gate.wait(r)
		inboxTestWriteFinal(w, "Error answer")
	})
	inboxTestUseProvider(t, provider, model)

	registry := newToolElisionLookupRegistry(t)
	store := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"New question"}, "model": {model}, "tool": {"lookup"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "lookup")

	provider.waitForRequest(t, 1)
	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	requireToolCallElided(t, state.presentation, "error-call", true)
	requireContains(t, state.presentation, "tool-error", "archive unavailable")
	requireStoredToolCallElision(t, database, chatID, "error-call", true)
	if _, _, _, _, stored := snapshotToolCall(t, database, chatID, "error-call"); stored.Content != errorResult {
		t.Fatalf("stored error body changed: %q", stored.Content)
	}

	gate.releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Error answer", "archive unavailable")
	requireToolCallElided(t, terminal, "error-call", true)
}

func TestToolResultElisionMapsSteeringBoundaryIndexes(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const model = "steering-elision-model"
	chatID := seedChat(t, database, "Steering elision", []string{"inbox_test_probe"}, nil)
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: model, MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	gates := []*inboxTestGate{newInboxTestGate(), newInboxTestGate(), newInboxTestGate()}
	defer func() {
		for _, gate := range gates {
			gate.releaseNow()
		}
	}()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index >= len(gates) {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gates[index].wait(r)
		switch index {
		case 0:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "before-steer", Name: "inbox_test_probe", Key: "old"})
		case 1:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "after-steer", Name: "inbox_test_probe", Key: "new"})
		default:
			inboxTestWriteFinal(w, "Finished steered elision")
		}
	})
	inboxTestUseProvider(t, provider, model)

	registry := inboxTestRegistry(t)
	store := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"Initial question"}, "model": {model}, "tool": {"inbox_test_probe"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "inbox_test_probe")

	provider.waitForRequest(t, 1)
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message": {"Steering question"}, "model": {model}, "tool": {"inbox_test_probe"},
	})
	gates[0].releaseNow()
	provider.waitForRequest(t, 2)

	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	requireToolCallElided(t, state.presentation, "before-steer", true)
	requireStoredToolCallElision(t, database, chatID, "before-steer", true)
	if _, _, _, _, stored := snapshotToolCall(t, database, chatID, "before-steer"); stored.Content != "probe:old" {
		t.Fatalf("pre-steer result body changed before the steer turn: %q", stored.Content)
	}

	gates[1].releaseNow()
	provider.waitForRequest(t, 3)
	state = tracker.snapshot()
	requireToolCallElided(t, state.presentation, "before-steer", true)
	if len(state.calls) != 1 || state.calls[0].ID != "after-steer" || state.calls[0].ResultElided {
		t.Fatalf("live calls after steer checkpoint = %#v, want full after-steer", state.calls)
	}
	requireStoredToolCallElision(t, database, chatID, "before-steer", true)
	requireStoredToolCallElision(t, database, chatID, "after-steer", false)
	for callID, want := range map[string]string{"before-steer": "probe:old", "after-steer": "probe:new"} {
		if _, _, _, _, stored := snapshotToolCall(t, database, chatID, callID); stored.Content != want {
			t.Fatalf("stored result body for %q = %q, want %q", callID, stored.Content, want)
		}
	}

	gates[2].releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Finished steered elision")
	requireToolCallElided(t, terminal, "before-steer", true)
	requireToolCallElided(t, terminal, "after-steer", false)
}

func TestToolResultElisionClearsWhenDisabled(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const (
		chatID = 86
		model  = "clear-elision-model"
	)
	oldResultID, oldResult := seedToolElisionHistory(t, database, chatID)
	found, messageID, callIndex, _, _ := snapshotToolCall(t, database, chatID, "old-call")
	if !found {
		t.Fatal("seeded old call missing")
	}
	if err := kritui_db.SetToolCallElisions(context.Background(), database, chatID, []kritui_db.ToolCallElision{{
		MessageID: messageID, Position: callIndex, Elided: true,
	}}); err != nil {
		t.Fatalf("seed elision flag: %v", err)
	}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: model, MaxToolRounds: 16,
		ToolResultElision: &llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone},
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	gate := newInboxTestGate()
	defer gate.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index != 0 {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gate.wait(r)
		inboxTestWriteFinal(w, "Cleared answer")
	})
	inboxTestUseProvider(t, provider, model)

	registry := newToolElisionLookupRegistry(t)
	store := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"Another question"}, "model": {model}, "tool": {"lookup"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "lookup")

	provider.waitForRequest(t, 1)
	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	requireToolCallElided(t, state.presentation, "old-call", false)
	requireStoredToolCallElision(t, database, chatID, "old-call", false)
	if _, _, _, _, stored := snapshotToolCall(t, database, chatID, "old-call"); stored.ID != oldResultID || stored.Content != oldResult {
		t.Fatalf("cleared result body changed: got ID %d content %q", stored.ID, stored.Content)
	}

	gate.releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Cleared answer")
	requireToolCallElided(t, terminal, "old-call", false)
}
