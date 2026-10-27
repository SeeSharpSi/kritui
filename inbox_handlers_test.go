package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/tools"
)

type inboxTestProbeTool struct{}

func (inboxTestProbeTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "inbox_test_probe",
		Description: "Returns a deterministic probe result",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`),
	}
}

func (inboxTestProbeTool) Execute(_ context.Context, arguments json.RawMessage) (string, error) {
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return "", fmt.Errorf("decode probe arguments: %w", err)
	}
	return "probe:" + payload.Key, nil
}

type inboxTestSecondaryTool struct{}

func (inboxTestSecondaryTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "inbox_test_other",
		Description: "Returns a deterministic secondary probe result",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`),
	}
}

func (inboxTestSecondaryTool) Execute(_ context.Context, arguments json.RawMessage) (string, error) {
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return "", fmt.Errorf("decode secondary probe arguments: %w", err)
	}
	return "other:" + payload.Key, nil
}

type inboxTestCountingProbeTool struct {
	calls *atomic.Int32
}

func (inboxTestCountingProbeTool) Definition() tools.Definition {
	return inboxTestProbeTool{}.Definition()
}

func (t inboxTestCountingProbeTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	t.calls.Add(1)
	return inboxTestProbeTool{}.Execute(ctx, arguments)
}

func inboxTestRegistry(t *testing.T) *tools.Registry {
	t.Helper()

	registry, err := tools.NewRegistry(inboxTestProbeTool{})
	if err != nil {
		t.Fatalf("create inbox test registry: %v", err)
	}
	return registry
}

func inboxTestDualRegistry(t *testing.T) *tools.Registry {
	t.Helper()

	registry, err := tools.NewRegistry(inboxTestProbeTool{}, inboxTestSecondaryTool{})
	if err != nil {
		t.Fatalf("create dual inbox test registry: %v", err)
	}
	return registry
}

func inboxTestCountingRegistry(t *testing.T, calls *atomic.Int32) *tools.Registry {
	t.Helper()

	registry, err := tools.NewRegistry(inboxTestCountingProbeTool{calls: calls})
	if err != nil {
		t.Fatalf("create counting inbox test registry: %v", err)
	}
	return registry
}

type inboxTestProviderRequest struct {
	Model       string
	Messages    []llm.Message
	RawMessages []json.RawMessage
	Tools       []string
}

type inboxTestChatFile struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type inboxTestChatContentPart struct {
	Type string             `json:"type"`
	Text string             `json:"text"`
	File *inboxTestChatFile `json:"file"`
}

type inboxTestRawChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func inboxTestUserContentParts(t *testing.T, request inboxTestProviderRequest, text string) []inboxTestChatContentPart {
	t.Helper()

	for _, raw := range request.RawMessages {
		var message inboxTestRawChatMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("decode raw provider message: %v", err)
		}
		if message.Role != "user" || len(message.Content) == 0 {
			continue
		}
		var parts []inboxTestChatContentPart
		if message.Content[0] == '"' {
			var plain string
			if err := json.Unmarshal(message.Content, &plain); err != nil {
				t.Fatalf("decode plain provider content: %v", err)
			}
			parts = []inboxTestChatContentPart{{Type: "text", Text: plain}}
		} else if err := json.Unmarshal(message.Content, &parts); err != nil {
			t.Fatalf("decode provider content parts: %v", err)
		}
		for _, part := range parts {
			if part.Type == "text" && strings.Contains(part.Text, text) {
				return parts
			}
		}
	}
	t.Fatalf("provider request has no user content matching %q: %#v", text, request.RawMessages)
	return nil
}

type inboxTestProvider struct {
	mu       sync.Mutex
	requests []inboxTestProviderRequest
	arrivals chan int
	script   func(index int, request inboxTestProviderRequest, requestHTTP *http.Request, w http.ResponseWriter)
	server   *httptest.Server
}

func newInboxTestProvider(t *testing.T, script func(index int, request inboxTestProviderRequest, requestHTTP *http.Request, w http.ResponseWriter)) *inboxTestProvider {
	t.Helper()

	provider := &inboxTestProvider{
		arrivals: make(chan int, 64),
		script:   script,
	}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serveHTTP))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *inboxTestProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		return
	}
	var payload struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid provider request", http.StatusBadRequest)
		return
	}

	request := inboxTestProviderRequest{Model: payload.Model, RawMessages: payload.Messages}
	for _, raw := range payload.Messages {
		var message llm.Message
		if err := json.Unmarshal(raw, &message); err != nil {
			var envelope struct {
				Role       string         `json:"role"`
				ToolCalls  []llm.ToolCall `json:"tool_calls"`
				ToolCallID string         `json:"tool_call_id"`
			}
			if envelopeErr := json.Unmarshal(raw, &envelope); envelopeErr != nil {
				http.Error(w, "invalid provider message", http.StatusBadRequest)
				return
			}
			message.Role = envelope.Role
			message.ToolCalls = envelope.ToolCalls
			message.ToolCallID = envelope.ToolCallID
		}
		request.Messages = append(request.Messages, message)
	}
	for _, tool := range payload.Tools {
		request.Tools = append(request.Tools, tool.Function.Name)
	}
	p.mu.Lock()
	p.requests = append(p.requests, request)
	index := len(p.requests) - 1
	p.mu.Unlock()

	p.arrivals <- index + 1
	p.script(index, request, r, w)
}

func (p *inboxTestProvider) waitForRequest(t *testing.T, number int) inboxTestProviderRequest {
	t.Helper()

	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case arrived := <-p.arrivals:
			if arrived > number {
				t.Fatalf("provider request %d arrived while waiting for request %d", arrived, number)
			}
			if arrived == number {
				p.mu.Lock()
				request := p.requests[number-1]
				p.mu.Unlock()
				return request
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for provider request %d", number)
		}
	}
}

func (p *inboxTestProvider) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func inboxTestUseProvider(t *testing.T, provider *inboxTestProvider, model string) {
	t.Helper()

	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", model)
	t.Setenv("LLM_ENDPOINT", provider.server.URL+"/provider")
}

func inboxTestWriteFinal(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "inbox-model",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
	})
}

type inboxTestToolCall struct {
	ID   string
	Name string
	Key  string
}

func inboxTestWriteToolCalls(w http.ResponseWriter, calls ...inboxTestToolCall) {
	toolCalls := make([]any, 0, len(calls))
	for _, call := range calls {
		arguments, _ := json.Marshal(map[string]string{"key": call.Key})
		toolCalls = append(toolCalls, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]string{
				"name":      call.Name,
				"arguments": string(arguments),
			},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "inbox-model",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
			"finish_reason": "tool_calls",
		}},
	})
}

type inboxTestGate struct {
	release chan struct{}
	once    sync.Once
}

func newInboxTestGate() *inboxTestGate {
	return &inboxTestGate{release: make(chan struct{})}
}

func (g *inboxTestGate) wait(r *http.Request) {
	select {
	case <-g.release:
	case <-r.Context().Done():
	}
}

func (g *inboxTestGate) releaseNow() {
	g.once.Do(func() { close(g.release) })
}

func inboxTestChatTarget(chatID int64) string {
	return "/messages?chat=" + strconv.FormatInt(chatID, 10)
}

func inboxTestCompletionTarget(chatID int64) string {
	return "/messages/complete?chat=" + strconv.FormatInt(chatID, 10)
}

func inboxTestRetryTarget(chatID int64) string {
	return "/messages/retry?chat=" + strconv.FormatInt(chatID, 10)
}

const inboxTestRequestMarker = `name="request" value="`

func inboxTestRequestID(t *testing.T, body string) string {
	t.Helper()

	_, remainder, ok := strings.Cut(body, inboxTestRequestMarker)
	if !ok {
		t.Fatalf("response does not contain a completion request ID: %s", body)
	}
	requestID, _, ok := strings.Cut(remainder, `"`)
	if !ok {
		t.Fatalf("response completion request ID is unterminated: %s", body)
	}
	if requestID == "" {
		t.Fatalf("response completion request ID is empty: %s", body)
	}
	return requestID
}

func inboxTestHasRequestID(body string) bool {
	return strings.Contains(body, inboxTestRequestMarker)
}

func inboxTestSubmitInitial(t *testing.T, handler http.Handler, chatID int64, form url.Values) string {
	t.Helper()

	response := postForm(t, handler, inboxTestChatTarget(chatID), form)
	if response.Code != http.StatusOK {
		t.Fatalf("initial message status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	return inboxTestRequestID(t, response.Body.String())
}

func inboxTestEnqueueActive(t *testing.T, handler http.Handler, chatID int64, form url.Values) string {
	t.Helper()

	response := postForm(t, handler, inboxTestChatTarget(chatID), form)
	if response.Code != http.StatusOK {
		t.Fatalf("pending message status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `id="prompt-inbox"`) {
		t.Fatalf("pending message response does not contain the prompt inbox: %s", body)
	}
	return body
}

func inboxTestStartCompletion(t *testing.T, handler http.Handler, chatID int64, requestID, model string, toolNames ...string) {
	t.Helper()

	form := url.Values{
		"model":   {model},
		"request": {requestID},
	}
	for _, name := range toolNames {
		form.Add("tool", name)
	}
	response := postForm(t, handler, inboxTestCompletionTarget(chatID), form)
	if response.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func inboxTestInbox(t *testing.T, ctx context.Context, database *sql.DB, chatID int64) []kritui_db.InboxItem {
	t.Helper()

	items, err := kritui_db.GetInbox(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get pending inbox: %v", err)
	}
	return items
}

func inboxTestMessages(t *testing.T, ctx context.Context, database *sql.DB, chatID int64) []llm.Message {
	t.Helper()

	messages, err := kritui_db.GetMessages(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get stored messages: %v", err)
	}
	return messages
}

func inboxTestSignatures(messages []llm.Message) []string {
	signatures := make([]string, 0, len(messages))
	for _, message := range messages {
		if len(message.ToolCalls) > 0 {
			names := make([]string, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				names = append(names, call.Function.Name)
			}
			signatures = append(signatures, message.Role+"(tool_calls="+strings.Join(names, ",")+")")
			continue
		}
		signatures = append(signatures, message.Role+":"+message.Content)
	}
	return signatures
}

func inboxTestRequireMessages(t *testing.T, messages []llm.Message, want []string) {
	t.Helper()

	if got := inboxTestSignatures(messages); !slices.Equal(got, want) {
		t.Fatalf("stored messages = %#v, want %#v", got, want)
	}
	lastID := int64(0)
	for index, message := range messages {
		if message.ID <= 0 {
			t.Fatalf("stored message %d ID = %d, want positive", index, message.ID)
		}
		if message.ID <= lastID {
			t.Fatalf("stored message %d ID = %d after %d, want increasing IDs", index, message.ID, lastID)
		}
		lastID = message.ID
	}
}

func inboxTestMessageIndex(messages []llm.Message, role, content string) int {
	for index, message := range messages {
		if message.Role == role && message.Content == content {
			return index
		}
	}
	return -1
}

func inboxTestCountUser(messages []llm.Message, content string) int {
	count := 0
	for _, message := range messages {
		if message.Role == "user" && message.Content == content {
			count++
		}
	}
	return count
}

func inboxTestPending(t *testing.T, items []kritui_db.InboxItem, wantSubmissions, wantDelivery []string) {
	t.Helper()

	if len(items) != len(wantSubmissions) {
		t.Fatalf("pending inbox item count = %d, want %d; items = %#v", len(items), len(wantSubmissions), items)
	}
	for index, item := range items {
		if item.SubmissionID != wantSubmissions[index] {
			t.Errorf("pending inbox item %d submission = %q, want %q", index, item.SubmissionID, wantSubmissions[index])
		}
		if item.Delivery != wantDelivery[index] {
			t.Errorf("pending inbox item %d delivery = %q, want %q", index, item.Delivery, wantDelivery[index])
		}
	}
}

func TestInboxQueueFIFOAfterActiveTurnWithToolRound(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox queue", []string{"inbox_test_probe"}, nil)
	registry := inboxTestRegistry(t)

	firstRequest := newInboxTestGate()
	defer firstRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			firstRequest.wait(r)
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "call-a", Name: "inbox_test_probe", Key: "alpha"})
		case 1:
			inboxTestWriteFinal(w, "Answer A")
		case 2:
			inboxTestWriteFinal(w, "Answer B")
		case 3:
			inboxTestWriteFinal(w, "Answer C")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question A"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model, "inbox_test_probe")
	provider.waitForRequest(t, 1)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued B"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"delivery":   {"queue"},
		"submission": {"submission-b"},
	})
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued C"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"delivery":   {"queue"},
		"submission": {"submission-c"},
	})
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-b", "submission-c"},
		[]string{"queue", "queue"},
	)

	firstRequest.releaseNow()

	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer C")

	if count := provider.requestCount(); count != 4 {
		t.Fatalf("provider request count = %d, want 4", count)
	}
	secondRequest := provider.waitForRequest(t, 2)
	thirdRequest := provider.waitForRequest(t, 3)
	fourthRequest := provider.waitForRequest(t, 4)
	if inboxTestMessageIndex(secondRequest.Messages, "user", "Queued B") >= 0 ||
		inboxTestMessageIndex(secondRequest.Messages, "user", "Queued C") >= 0 {
		t.Fatalf("queued prompts leaked into the A tool continuation: %#v", inboxTestSignatures(secondRequest.Messages))
	}
	if inboxTestMessageIndex(thirdRequest.Messages, "user", "Queued B") < 0 {
		t.Fatalf("B was not delivered before its provider request: %#v", inboxTestSignatures(thirdRequest.Messages))
	}
	if inboxTestMessageIndex(fourthRequest.Messages, "user", "Queued C") < 0 {
		t.Fatalf("C was not delivered before its provider request: %#v", inboxTestSignatures(fourthRequest.Messages))
	}

	inboxTestRequireMessages(t, inboxTestMessages(t, ctx, database, chatID), []string{
		"user:Question A",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:alpha",
		"assistant:Answer A",
		"user:Queued B",
		"assistant:Answer B",
		"user:Queued C",
		"assistant:Answer C",
	})
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after queue drain = %#v, want empty", pending)
	}
	state, err := kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get completion state: %v", err)
	}
	if state.Pending {
		t.Fatal("completion state is still pending after the queue drained")
	}
}

func TestInboxSteeringWaitsForEntireToolBatch(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox steering tools", []string{"inbox_test_probe"}, nil)
	registry := inboxTestRegistry(t)

	firstRequest := newInboxTestGate()
	defer firstRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			firstRequest.wait(r)
			inboxTestWriteToolCalls(w,
				inboxTestToolCall{ID: "call-1", Name: "inbox_test_probe", Key: "one"},
				inboxTestToolCall{ID: "call-2", Name: "inbox_test_probe", Key: "two"},
			)
		case 1:
			inboxTestWriteFinal(w, "Answer after steer")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question with tools"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-tools"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model, "inbox_test_probe")
	first := provider.waitForRequest(t, 1)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Steer after tool results"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-steer-tools"},
	})
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-steer-tools"},
		[]string{"steer"},
	)

	firstRequest.releaseNow()
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer after steer")

	if count := provider.requestCount(); count != 2 {
		t.Fatalf("provider request count = %d, want 2", count)
	}
	second := provider.waitForRequest(t, 2)
	if inboxTestMessageIndex(first.Messages, "user", "Steer after tool results") >= 0 {
		t.Fatal("steering text appeared before the tool batch completed")
	}
	firstResult := inboxTestMessageIndex(second.Messages, "tool", "probe:one")
	secondResult := inboxTestMessageIndex(second.Messages, "tool", "probe:two")
	steering := inboxTestMessageIndex(second.Messages, "user", "Steer after tool results")
	if firstResult < 0 || secondResult < 0 {
		t.Fatalf("tool results missing from steering continuation: %#v", inboxTestSignatures(second.Messages))
	}
	if steering < 0 {
		t.Fatalf("steering prompt missing from steering continuation: %#v", inboxTestSignatures(second.Messages))
	}
	if steering < firstResult || steering < secondResult {
		t.Fatalf("steering prompt index %d does not follow tool result indexes %d and %d: %#v", steering, firstResult, secondResult, inboxTestSignatures(second.Messages))
	}

	inboxTestRequireMessages(t, inboxTestMessages(t, ctx, database, chatID), []string{
		"user:Question with tools",
		"assistant(tool_calls=inbox_test_probe,inbox_test_probe)",
		"tool:probe:one",
		"tool:probe:two",
		"user:Steer after tool results",
		"assistant:Answer after steer",
	})
}

func TestInboxSteeringDuringFinalResponseContinues(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox steering final", nil, nil)
	registry := inboxTestRegistry(t)

	firstRequest := newInboxTestGate()
	defer firstRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			firstRequest.wait(r)
			inboxTestWriteFinal(w, "Answer A")
		case 1:
			inboxTestWriteFinal(w, "Answer after late steer")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question A"},
		"model":      {model},
		"submission": {"submission-final-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model)
	first := provider.waitForRequest(t, 1)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Late steer"},
		"model":      {model},
		"submission": {"submission-late-steer"},
	})
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-late-steer"},
		[]string{"steer"},
	)

	firstRequest.releaseNow()
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer after late steer")

	if count := provider.requestCount(); count != 2 {
		t.Fatalf("provider request count = %d, want 2", count)
	}
	second := provider.waitForRequest(t, 2)
	if inboxTestMessageIndex(first.Messages, "user", "Late steer") >= 0 {
		t.Fatal("late steering text appeared in the already-running request")
	}
	answerIndex := inboxTestMessageIndex(second.Messages, "assistant", "Answer A")
	steerIndex := inboxTestMessageIndex(second.Messages, "user", "Late steer")
	if answerIndex < 0 || steerIndex < 0 || steerIndex < answerIndex {
		t.Fatalf("late steering was not appended after the final response: %#v", inboxTestSignatures(second.Messages))
	}

	inboxTestRequireMessages(t, inboxTestMessages(t, ctx, database, chatID), []string{
		"user:Question A",
		"assistant:Answer A",
		"user:Late steer",
		"assistant:Answer after late steer",
	})
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after late steering = %#v, want empty", pending)
	}
}

func TestInboxDuplicateSubmissionIDAcceptedOnce(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox duplicate", nil, nil)
	registry := inboxTestRegistry(t)

	firstRequest := newInboxTestGate()
	defer firstRequest.releaseNow()
	thirdRequest := newInboxTestGate()
	defer thirdRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			firstRequest.wait(r)
			inboxTestWriteFinal(w, "Answer A")
		case 1:
			inboxTestWriteFinal(w, "Answer B")
		case 2:
			thirdRequest.wait(r)
			inboxTestWriteFinal(w, "Answer C")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question A"},
		"model":      {model},
		"submission": {"submission-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model)
	provider.waitForRequest(t, 1)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued B"},
		"model":      {model},
		"delivery":   {"queue"},
		"submission": {"submission-duplicate"},
	})
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued B duplicate"},
		"model":      {model},
		"delivery":   {"queue"},
		"submission": {"submission-duplicate"},
	})
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued C"},
		"model":      {model},
		"delivery":   {"queue"},
		"submission": {"submission-c"},
	})
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-duplicate", "submission-c"},
		[]string{"queue", "queue"},
	)

	firstRequest.releaseNow()
	provider.waitForRequest(t, 3)
	if count := provider.requestCount(); count != 3 {
		t.Fatalf("provider request count while C is active = %d, want 3", count)
	}
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox while C is active = %#v, want empty after delivery", pending)
	}

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued B duplicate after delivery"},
		"model":      {model},
		"delivery":   {"queue"},
		"submission": {"submission-duplicate"},
	})
	if count := provider.requestCount(); count != 3 {
		t.Fatalf("duplicate after delivery started provider request %d, want still 3", count)
	}

	thirdRequest.releaseNow()
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer C")

	if count := provider.requestCount(); count != 3 {
		t.Fatalf("provider request count after duplicate delivery = %d, want 3", count)
	}
	messages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, messages, []string{
		"user:Question A",
		"assistant:Answer A",
		"user:Queued B",
		"assistant:Answer B",
		"user:Queued C",
		"assistant:Answer C",
	})
	if count := inboxTestCountUser(messages, "Queued B"); count != 1 {
		t.Fatalf("stored Queued B count = %d, want 1", count)
	}
	if count := inboxTestCountUser(messages, "Queued B duplicate after delivery"); count != 0 {
		t.Fatalf("duplicate replay stored an extra user message: %q", "Queued B duplicate after delivery")
	}
}

func TestInboxProviderFailureLeavesQueuePendingForRetry(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox retry", []string{"inbox_test_probe"}, nil)
	registry := inboxTestRegistry(t)

	failureRequest := newInboxTestGate()
	defer failureRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "call-failure", Name: "inbox_test_probe", Key: "retry"})
		case 1:
			failureRequest.wait(r)
			http.Error(w, "upstream failed", http.StatusInternalServerError)
		case 2:
			inboxTestWriteFinal(w, "Answer A after retry")
		case 3:
			inboxTestWriteFinal(w, "Answer B after retry")
		case 4:
			inboxTestWriteFinal(w, "Answer C after retry")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)
	retry := messageRetryHandler(database, registry, toolCalls)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question A"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model, "inbox_test_probe")
	provider.waitForRequest(t, 2)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued B"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"delivery":   {"queue"},
		"submission": {"submission-b"},
	})
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued C"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"delivery":   {"queue"},
		"submission": {"submission-c"},
	})

	failureRequest.releaseNow()
	failedTerminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, failedTerminal, "HTTP 500")

	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-b", "submission-c"},
		[]string{"queue", "queue"},
	)
	failedMessages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, failedMessages, []string{
		"user:Question A",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:retry",
	})
	state, err := kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get failed completion state: %v", err)
	}
	if !state.Pending {
		t.Fatal("completion state is not pending after a provider failure with a trailing tool result")
	}

	retryResponse := postForm(t, retry, inboxTestRetryTarget(chatID), url.Values{
		"model": {model},
		"tool":  {"inbox_test_probe"},
	})
	if retryResponse.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d; body = %q", retryResponse.Code, http.StatusOK, retryResponse.Body.String())
	}
	retryRequestID := inboxTestRequestID(t, retryResponse.Body.String())
	if retryRequestID == requestID {
		t.Fatal("retry reused the failed completion request ID")
	}
	inboxTestStartCompletion(t, completion, chatID, retryRequestID, model, "inbox_test_probe")
	retryTerminal := waitForCompletionResult(t, toolCalls, retryRequestID)
	requireContains(t, retryTerminal, "Answer C after retry")

	if count := provider.requestCount(); count != 5 {
		t.Fatalf("provider request count after retry = %d, want 5", count)
	}
	retryRequest := provider.waitForRequest(t, 3)
	if inboxTestMessageIndex(retryRequest.Messages, "tool", "probe:retry") < 0 {
		t.Fatalf("retry request did not resume from the trailing tool result: %#v", inboxTestSignatures(retryRequest.Messages))
	}

	messages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, messages, []string{
		"user:Question A",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:retry",
		"assistant:Answer A after retry",
		"user:Queued B",
		"assistant:Answer B after retry",
		"user:Queued C",
		"assistant:Answer C after retry",
	})
	if count := inboxTestCountUser(messages, "Queued B"); count != 1 {
		t.Fatalf("stored Queued B count after retry = %d, want 1", count)
	}
	if count := inboxTestCountUser(messages, "Queued C"); count != 1 {
		t.Fatalf("stored Queued C count after retry = %d, want 1", count)
	}
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after retry drained = %#v, want empty", pending)
	}
	state, err = kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get completion state after retry: %v", err)
	}
	if state.Pending {
		t.Fatal("completion state is still pending after retry drained the queue")
	}
}

func TestInboxSteeringPDFAttachmentUsesPersistedPromptContext(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "vision-model"
	const appendID = "inbox-pdf-append"
	const appendText = "PDF append instruction appears exactly once."
	seedPromptAppends(t, database, []kritui_db.PromptAppend{{
		ID: appendID, Name: "PDF review", Text: appendText,
	}})
	chatID := seedChat(t, database, "Inbox PDF steering", nil, nil)
	registry := inboxTestRegistry(t)

	initialRequest := newInboxTestGate()
	defer initialRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			initialRequest.wait(r)
			inboxTestWriteFinal(w, "Initial answer")
		case 1:
			inboxTestWriteFinal(w, "Answer with PDF")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question before PDF"},
		"model":      {model},
		"submission": {"submission-initial-pdf"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model)
	provider.waitForRequest(t, 1)

	pdf := testPDFBytes()
	body, contentType := multipartImageRequest(t, "Steer with PDF", []testUpload{{
		name: "steering-report.pdf", contentType: "application/pdf", data: pdf,
	}})
	query := url.Values{
		"chat":       {strconv.FormatInt(chatID, 10)},
		"delivery":   {"steer"},
		"submission": {"submission-pdf"},
		"append":     {appendID},
	}
	request := httptest.NewRequest(http.MethodPost, "/messages?"+query.Encode(), bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	message.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PDF steering status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), `id="prompt-inbox"`, "Steer with PDF")

	items := inboxTestInbox(t, ctx, database, chatID)
	inboxTestPending(t, items, []string{"submission-pdf"}, []string{"steer"})
	item := items[0]
	if len(item.AppendIDs) != 1 || item.AppendIDs[0] != appendID {
		t.Fatalf("pending PDF append IDs = %#v, want [%q]", item.AppendIDs, appendID)
	}
	if len(item.Message.Attachments) != 1 {
		t.Fatalf("pending PDF attachments = %#v, want one", item.Message.Attachments)
	}
	pendingAttachment := item.Message.Attachments[0]
	if pendingAttachment.Filename != "steering-report.pdf" || pendingAttachment.MediaType != "application/pdf" || !bytes.Equal(pendingAttachment.Data, pdf) {
		t.Fatalf("pending PDF attachment = %#v", pendingAttachment)
	}
	if !slices.Equal(item.Message.PromptAppendTexts, []string{appendText}) {
		t.Fatalf("pending PDF append texts = %#v, want [%q]", item.Message.PromptAppendTexts, appendText)
	}

	initialRequest.releaseNow()
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer with PDF")

	if count := provider.requestCount(); count != 2 {
		t.Fatalf("provider request count = %d, want 2", count)
	}
	second := provider.waitForRequest(t, 2)
	parts := inboxTestUserContentParts(t, second, "Steer with PDF")
	var filePart *inboxTestChatFile
	var userText strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			userText.WriteString(part.Text)
		}
		if part.Type == "file" && part.File != nil {
			filePart = part.File
		}
	}
	if filePart == nil {
		t.Fatalf("steering request did not include a PDF file part: %#v", parts)
	}
	if filePart.Filename != "steering-report.pdf" {
		t.Errorf("steering PDF filename = %q, want %q", filePart.Filename, "steering-report.pdf")
	}
	if !strings.HasPrefix(filePart.FileData, "data:application/pdf;base64,") {
		t.Errorf("steering PDF file_data prefix = %q, want application/pdf base64", filePart.FileData[:min(len(filePart.FileData), 40)])
	}
	if count := strings.Count(userText.String(), appendText); count != 1 {
		t.Fatalf("steering prompt append text count = %d, want 1; text = %q", count, userText.String())
	}

	messages := inboxTestMessages(t, ctx, database, chatID)
	var pdfMessage llm.Message
	for _, stored := range messages {
		for _, attachment := range stored.Attachments {
			if attachment.Filename == "steering-report.pdf" {
				pdfMessage = stored
			}
		}
	}
	if pdfMessage.ID <= 0 {
		t.Fatalf("stored PDF steering message ID = %d, want positive; messages = %#v", pdfMessage.ID, messages)
	}
	if len(second.Messages) == 0 || second.Messages[0].Role != "system" {
		t.Fatalf("steering request system prompt missing: %#v", inboxTestSignatures(second.Messages))
	}
	wantURL := "/chats/" + strconv.FormatInt(chatID, 10) + "/messages/" + strconv.FormatInt(pdfMessage.ID, 10) + "/attachments/0"
	if !strings.Contains(second.Messages[0].Content, wantURL) {
		t.Fatalf("system prompt does not contain canonical PDF URL %q: %s", wantURL, second.Messages[0].Content)
	}
	requireContains(t, second.Messages[0].Content, `"steering-report.pdf"`)

	attachmentResponse := serveAttachmentRequest(
		t, messageAttachmentHandler(database), kritui_db.WithUser(ctx, kritui_db.LegacyOwnerID),
		http.MethodGet, strconv.FormatInt(chatID, 10), strconv.FormatInt(pdfMessage.ID, 10), "0", nil,
	)
	if attachmentResponse.Code != http.StatusOK {
		t.Fatalf("owner PDF status = %d, want %d; body = %q", attachmentResponse.Code, http.StatusOK, attachmentResponse.Body.String())
	}
	requireInlinePDFDisposition(t, attachmentResponse.Header().Get("Content-Disposition"), "steering-report.pdf")
	if !bytes.Equal(attachmentResponse.Body.Bytes(), pdf) {
		t.Fatalf("owner PDF body = %q, want %q", attachmentResponse.Body.Bytes(), pdf)
	}

	inboxTestRequireMessages(t, messages, []string{
		"user:Question before PDF",
		"assistant:Initial answer",
		"user:Steer with PDF",
		"assistant:Answer with PDF",
	})
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after PDF steering = %#v, want empty", pending)
	}
}

func TestInboxToolBatchFailureRetryUsesStoredStateOnly(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox interrupted tools", []string{"inbox_test_probe"}, nil)
	var executions atomic.Int32
	registry := inboxTestCountingRegistry(t, &executions)

	failureRequest := newInboxTestGate()
	defer failureRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "call-stored", Name: "inbox_test_probe", Key: "stored"})
		case 1:
			failureRequest.wait(r)
			http.Error(w, "upstream failed after tool batch", http.StatusInternalServerError)
		case 2:
			inboxTestWriteFinal(w, "Answer A after stored retry")
		case 3:
			inboxTestWriteFinal(w, "Answer B after stored retry")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)
	retry := messageRetryHandler(database, registry, toolCalls)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Question with stored tools"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-stored-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, model, "inbox_test_probe")
	provider.waitForRequest(t, 2)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued after stored retry"},
		"model":      {model},
		"tool":       {"inbox_test_probe"},
		"delivery":   {"queue"},
		"submission": {"submission-stored-b"},
	})
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-stored-b"},
		[]string{"queue"},
	)

	failureRequest.releaseNow()
	failedTerminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, failedTerminal, "HTTP 500")

	if count := executions.Load(); count != 1 {
		t.Fatalf("tool executions after the successful batch and failure = %d, want 1", count)
	}
	failedMessages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, failedMessages, []string{
		"user:Question with stored tools",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:stored",
	})
	if last := failedMessages[len(failedMessages)-1]; last.Role != "tool" || last.Content != "probe:stored" {
		t.Fatalf("stored history does not end with the successful tool result: %#v", inboxTestSignatures(failedMessages))
	}
	state, err := kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get interrupted completion state: %v", err)
	}
	if !state.Pending {
		t.Fatal("completion state is not pending after the provider failed following a tool batch")
	}
	if state.Model != model || !slices.Equal(state.Tools, []string{"inbox_test_probe"}) {
		t.Fatalf("interrupted completion state = model %q tools %v, want model %q tools [inbox_test_probe]", state.Model, state.Tools, model)
	}
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"submission-stored-b"},
		[]string{"queue"},
	)

	home := homeHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	reloadRequest := httptest.NewRequest(http.MethodGet, "/?chat="+strconv.FormatInt(chatID, 10), nil)
	reloadResponse := httptest.NewRecorder()
	home.ServeHTTP(reloadResponse, reloadRequest)
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("reload status = %d, want %d; body = %q", reloadResponse.Code, http.StatusOK, reloadResponse.Body.String())
	}
	reloadBody := reloadResponse.Body.String()
	requireContains(t, reloadBody,
		"Completion was interrupted. Retry the completion.",
		`hx-post="/messages/retry?chat=`+strconv.FormatInt(chatID, 10)+`"`,
		`name="model" value="`+model+`"`,
		`name="tool" value="inbox_test_probe"`,
	)

	retryForm := url.Values{"model": {state.Model}}
	for _, name := range state.Tools {
		retryForm.Add("tool", name)
	}
	retryResponse := postForm(t, retry, inboxTestRetryTarget(chatID), retryForm)
	if retryResponse.Code != http.StatusOK {
		t.Fatalf("stored-state retry status = %d, want %d; body = %q", retryResponse.Code, http.StatusOK, retryResponse.Body.String())
	}
	retryRequestID := inboxTestRequestID(t, retryResponse.Body.String())
	if retryRequestID == requestID {
		t.Fatal("stored-state retry reused the failed completion request ID")
	}
	inboxTestStartCompletion(t, completion, chatID, retryRequestID, state.Model, state.Tools...)
	retryTerminal := waitForCompletionResult(t, toolCalls, retryRequestID)
	requireContains(t, retryTerminal, "Answer B after stored retry")

	if count := executions.Load(); count != 1 {
		t.Fatalf("tool executions after retry = %d, want 1 without reexecution", count)
	}
	if count := provider.requestCount(); count != 4 {
		t.Fatalf("provider request count after stored retry = %d, want 4", count)
	}
	retryProviderRequest := provider.waitForRequest(t, 3)
	if inboxTestMessageIndex(retryProviderRequest.Messages, "tool", "probe:stored") < 0 {
		t.Fatalf("retry request did not resume from the stored tool result: %#v", inboxTestSignatures(retryProviderRequest.Messages))
	}
	if !slices.Equal(retryProviderRequest.Tools, []string{"inbox_test_probe"}) {
		t.Fatalf("retry tools = %v, want the stored tool snapshot [inbox_test_probe]", retryProviderRequest.Tools)
	}
	queuedProviderRequest := provider.waitForRequest(t, 4)
	if inboxTestMessageIndex(queuedProviderRequest.Messages, "user", "Queued after stored retry") < 0 {
		t.Fatalf("queued prompt was not delivered after the retried final answer: %#v", inboxTestSignatures(queuedProviderRequest.Messages))
	}

	messages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, messages, []string{
		"user:Question with stored tools",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:stored",
		"assistant:Answer A after stored retry",
		"user:Queued after stored retry",
		"assistant:Answer B after stored retry",
	})
	if count := inboxTestCountUser(messages, "Queued after stored retry"); count != 1 {
		t.Fatalf("stored queued prompt count after retry = %d, want 1", count)
	}
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after stored retry = %#v, want empty", pending)
	}
	state, err = kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get completion state after stored retry: %v", err)
	}
	if state.Pending {
		t.Fatal("completion state is still pending after the queued prompt drained")
	}
}

func TestInboxQueuedAndSteeringUseCapturedModelToolSnapshots(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const activeModel = "inbox-model-a"
	const queuedModel = "inbox-model-b"
	const steeringModel = "inbox-model-c"
	chatID := seedChat(t, database, "Inbox option snapshots", []string{"inbox_test_probe"}, nil)
	registry := inboxTestDualRegistry(t)

	initialRequest := newInboxTestGate()
	defer initialRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		switch index {
		case 0:
			initialRequest.wait(r)
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "call-active", Name: "inbox_test_probe", Key: "active"})
		case 1:
			inboxTestWriteFinal(w, "Answer after steering")
		case 2:
			inboxTestWriteFinal(w, "Answer from queued prompt")
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	})
	inboxTestUseProvider(t, provider, activeModel)

	toolCalls := newToolCallStore()
	message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
	completion := messageCompletionHandler(database, registry, toolCalls, nil)

	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message":    {"Active question"},
		"model":      {activeModel},
		"tool":       {"inbox_test_probe"},
		"submission": {"submission-snapshot-a"},
	})
	inboxTestStartCompletion(t, completion, chatID, requestID, activeModel, "inbox_test_probe")
	activeRequest := provider.waitForRequest(t, 1)

	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Queued with its own options"},
		"model":      {queuedModel},
		"tool":       {"inbox_test_other"},
		"delivery":   {"queue"},
		"submission": {"submission-snapshot-b"},
	})
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message":    {"Steer with different options"},
		"model":      {steeringModel},
		"tool":       {"inbox_test_other"},
		"submission": {"submission-snapshot-s"},
	})
	items := inboxTestInbox(t, ctx, database, chatID)
	inboxTestPending(t, items,
		[]string{"submission-snapshot-b", "submission-snapshot-s"},
		[]string{"queue", "steer"},
	)
	if items[0].Model != queuedModel || !slices.Equal(items[0].Tools, []string{"inbox_test_other"}) {
		t.Fatalf("queued snapshot = model %q tools %v, want model %q tools [inbox_test_other]", items[0].Model, items[0].Tools, queuedModel)
	}
	if items[1].Model != steeringModel || !slices.Equal(items[1].Tools, []string{"inbox_test_other"}) {
		t.Fatalf("steering snapshot = model %q tools %v, want submitted model %q tools [inbox_test_other]", items[1].Model, items[1].Tools, steeringModel)
	}

	initialRequest.releaseNow()
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer from queued prompt")

	if count := provider.requestCount(); count != 3 {
		t.Fatalf("provider request count = %d, want 3", count)
	}
	if activeRequest.Model != activeModel || !slices.Equal(activeRequest.Tools, []string{"inbox_test_probe"}) {
		t.Fatalf("active request = model %q tools %v, want model %q tools [inbox_test_probe]", activeRequest.Model, activeRequest.Tools, activeModel)
	}
	steeringRequest := provider.waitForRequest(t, 2)
	if steeringRequest.Model != activeModel || !slices.Equal(steeringRequest.Tools, []string{"inbox_test_probe"}) {
		t.Fatalf("steering continuation = model %q tools %v, want active model %q tools [inbox_test_probe]", steeringRequest.Model, steeringRequest.Tools, activeModel)
	}
	if inboxTestMessageIndex(steeringRequest.Messages, "user", "Queued with its own options") >= 0 {
		t.Fatalf("queued prompt leaked into the steering continuation: %#v", inboxTestSignatures(steeringRequest.Messages))
	}
	if inboxTestMessageIndex(steeringRequest.Messages, "user", "Steer with different options") < 0 {
		t.Fatalf("steering prompt missing from the active continuation: %#v", inboxTestSignatures(steeringRequest.Messages))
	}
	if inboxTestMessageIndex(steeringRequest.Messages, "tool", "probe:active") < 0 {
		t.Fatalf("steering continuation lost the active tool result: %#v", inboxTestSignatures(steeringRequest.Messages))
	}
	queuedRequest := provider.waitForRequest(t, 3)
	if queuedRequest.Model != queuedModel || !slices.Equal(queuedRequest.Tools, []string{"inbox_test_other"}) {
		t.Fatalf("queued request = model %q tools %v, want captured model %q tools [inbox_test_other]", queuedRequest.Model, queuedRequest.Tools, queuedModel)
	}
	if inboxTestMessageIndex(queuedRequest.Messages, "user", "Queued with its own options") < 0 {
		t.Fatalf("queued prompt missing from its own provider request: %#v", inboxTestSignatures(queuedRequest.Messages))
	}

	inboxTestRequireMessages(t, inboxTestMessages(t, ctx, database, chatID), []string{
		"user:Active question",
		"assistant(tool_calls=inbox_test_probe)",
		"tool:probe:active",
		"user:Steer with different options",
		"assistant:Answer after steering",
		"user:Queued with its own options",
		"assistant:Answer from queued prompt",
	})
	chatTools, err := kritui_db.GetChatTools(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get chat tools after queued prompt: %v", err)
	}
	if !slices.Equal(chatTools, []string{"inbox_test_other"}) {
		t.Fatalf("chat tools after queued prompt = %v, want [inbox_test_other]", chatTools)
	}
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after option snapshots = %#v, want empty", pending)
	}
}

func TestInboxMutationRejectsForeignAccount(t *testing.T) {
	database := openTestDatabase(t)
	const model = "inbox-model"

	wakeupRequest := newInboxTestGate()
	defer wakeupRequest.releaseNow()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index != 0 {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		wakeupRequest.wait(r)
		inboxTestWriteFinal(w, "Owner wakeup answer")
	})
	inboxTestUseProvider(t, provider, model)

	toolCalls := newToolCallStore()
	handler := newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		toolCalls,
		nil,
		newAuthService(database, false),
	)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newAuthTestClient()

	const ownerName = "Inbox_Owner"
	const intruderName = "Inbox_Intruder"
	_, ownerCookie := authSignup(t, client, server.URL, ownerName, authTestPassword, "")
	_, intruderCookie := authSignup(t, client, server.URL, intruderName, authTestPassword, "")
	owner, err := kritui_db.GetUserByUsername(context.Background(), database, ownerName)
	if err != nil {
		t.Fatalf("get owner account: %v", err)
	}
	if _, err := kritui_db.GetUserByUsername(context.Background(), database, intruderName); err != nil {
		t.Fatalf("get intruder account: %v", err)
	}

	ownerContext := kritui_db.WithUser(context.Background(), owner.ID)
	chatID, err := kritui_db.AllocateChat(ownerContext, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate owner chat: %v", err)
	}
	item, err := kritui_db.EnqueuePrompt(ownerContext, database, chatID, kritui_db.InboxItem{
		SubmissionID: "owner-secret-submission",
		Delivery:     "queue",
		Model:        model,
		Message:      llm.Message{Role: "user", Content: "Owner secret prompt"},
	})
	if err != nil {
		t.Fatalf("enqueue owner prompt: %v", err)
	}
	cancelledItem, err := kritui_db.EnqueuePrompt(ownerContext, database, chatID, kritui_db.InboxItem{
		SubmissionID: "owner-cancelled-submission",
		Delivery:     "queue",
		Model:        model,
		Message:      llm.Message{Role: "user", Content: "Owner cancelled prompt"},
	})
	if err != nil {
		t.Fatalf("enqueue owner cancellation prompt: %v", err)
	}

	target := "/chats/" + strconv.FormatInt(chatID, 10) + "/inbox/" + strconv.FormatInt(item.ID, 10)
	cancelTarget := "/chats/" + strconv.FormatInt(chatID, 10) + "/inbox/" + strconv.FormatInt(cancelledItem.ID, 10)
	resumeTarget := "/chats/" + strconv.FormatInt(chatID, 10) + "/inbox/resume"

	response, body := inboxTestAuthFormRequest(t, client, http.MethodPatch, server.URL+target, url.Values{"delivery": {"steer"}}, intruderCookie)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign PATCH status = %d, want %d; body = %q", response.StatusCode, http.StatusNotFound, body)
	}
	if retarget := response.Header.Get("HX-Retarget"); retarget != "#messages" {
		t.Errorf("foreign PATCH HX-Retarget = %q, want #messages", retarget)
	}
	requireContains(t, body, `role="alert"`, "Pending prompt is no longer available.")
	requireNotContains(t, body, `id="prompt-inbox"`)
	response, body = inboxTestAuthFormRequest(t, client, http.MethodDelete, server.URL+target, nil, intruderCookie)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign DELETE status = %d, want %d; body = %q", response.StatusCode, http.StatusNotFound, body)
	}
	if retarget := response.Header.Get("HX-Retarget"); retarget != "#messages" {
		t.Errorf("foreign DELETE HX-Retarget = %q, want #messages", retarget)
	}
	requireContains(t, body, `role="alert"`, "Pending prompt is no longer available.")
	requireNotContains(t, body, `id="prompt-inbox"`)
	response, body = inboxTestAuthFormRequest(t, client, http.MethodPost, server.URL+resumeTarget, nil, intruderCookie)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign resume status = %d, want %d; body = %q", response.StatusCode, http.StatusNotFound, body)
	}
	if count := provider.requestCount(); count != 0 {
		t.Fatalf("foreign mutation started %d provider requests, want 0", count)
	}
	inboxTestPending(t,
		inboxTestInbox(t, ownerContext, database, chatID),
		[]string{"owner-secret-submission", "owner-cancelled-submission"},
		[]string{"queue", "queue"},
	)

	response, body = inboxTestAuthFormRequest(t, client, http.MethodDelete, server.URL+cancelTarget, nil, ownerCookie)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner DELETE status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, body)
	}
	inboxTestPending(t,
		inboxTestInbox(t, ownerContext, database, chatID),
		[]string{"owner-secret-submission"},
		[]string{"queue"},
	)

	response, body = inboxTestAuthFormRequest(t, client, http.MethodPatch, server.URL+target, url.Values{"delivery": {"steer"}}, ownerCookie)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner wakeup PATCH status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, body)
	}
	if response.Header.Get("HX-Retarget") != "#messages" || response.Header.Get("HX-Reswap") != "beforeend" {
		t.Fatalf("owner wakeup headers = retarget %q swap %q, want #messages beforeend", response.Header.Get("HX-Retarget"), response.Header.Get("HX-Reswap"))
	}
	requireContains(t, body, `id="prompt-inbox"`, `id="completion-`, "Owner secret prompt")
	requestID := inboxTestRequestID(t, body)
	if pending := inboxTestInbox(t, ownerContext, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after owner wakeup delivery = %#v, want empty", pending)
	}

	provider.waitForRequest(t, 1)
	wakeupRequest.releaseNow()
	terminal := waitForOwnedCompletionResult(t, toolCalls, owner.ID, requestID)
	requireContains(t, terminal, "Owner wakeup answer")

	if count := provider.requestCount(); count != 1 {
		t.Fatalf("provider request count after owner wakeup = %d, want 1", count)
	}
	inboxTestRequireMessages(t, inboxTestMessages(t, ownerContext, database, chatID), []string{
		"user:Owner secret prompt",
		"assistant:Owner wakeup answer",
	})
	if active, ok := toolCalls.activeForUser(owner.ID, chatID); ok {
		t.Fatalf("owner wakeup left an active worker: %#v", active)
	}
}

func inboxTestMutationRequest(t *testing.T, handler http.Handler, method string, chatID, itemID int64, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	if values == nil {
		values = url.Values{}
	}
	request := httptest.NewRequest(method, "/chats/"+strconv.FormatInt(chatID, 10)+"/inbox/"+strconv.FormatInt(itemID, 10), strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetPathValue("chat", strconv.FormatInt(chatID, 10))
	request.SetPathValue("input", strconv.FormatInt(itemID, 10))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func inboxTestRequireConflictCheckpoint(t *testing.T, response *httptest.ResponseRecorder, pendingID int64, pendingText string) {
	t.Helper()

	if response.Code != http.StatusConflict {
		t.Fatalf("stale inbox mutation status = %d, want %d; body = %q", response.Code, http.StatusConflict, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Errorf("stale inbox mutation Content-Type = %q, want HTML", contentType)
	}
	if retarget := response.Header().Get("HX-Retarget"); retarget != "#messages" {
		t.Errorf("stale inbox mutation HX-Retarget = %q, want #messages", retarget)
	}
	if swap := response.Header().Get("HX-Reswap"); swap != "beforeend" {
		t.Errorf("stale inbox mutation HX-Reswap = %q, want beforeend", swap)
	}
	body := response.Body.String()
	requireContains(t, body,
		`role="alert"`,
		"Pending prompt is no longer available.",
		"<hx-partial",
		`hx-target="#prompt-inbox"`,
		`hx-swap="outerHTML"`,
		`id="prompt-inbox"`,
		`data-inbox-id="`+strconv.FormatInt(pendingID, 10)+`"`,
		pendingText,
	)
	if count := strings.Count(body, `id="prompt-inbox"`); count != 1 {
		t.Fatalf("stale inbox mutation rendered %d prompt-inbox panels, want 1: %s", count, body)
	}
	if errorIndex, panelIndex := strings.Index(body, "Pending prompt is no longer available."), strings.Index(body, `id="prompt-inbox"`); errorIndex < 0 || panelIndex < 0 || errorIndex > panelIndex {
		t.Fatalf("stale inbox mutation did not render the error before the inbox checkpoint: %s", body)
	}
}

func inboxTestAuthFormRequest(t *testing.T, client *http.Client, method, target string, values url.Values, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()

	var body *strings.Reader
	if values == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(values.Encode())
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	return authDo(t, client, request)
}

func TestInboxIdleSteerPatchStartsParkedQueueOnce(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox idle parked queue", nil, nil)
	registry := inboxTestRegistry(t)

	if _, err := kritui_db.InsertMessage(ctx, database, chatID, 0, llm.Message{
		Role: "assistant", Content: "Prior answer",
	}); err != nil {
		t.Fatalf("insert prior answer: %v", err)
	}
	item, err := kritui_db.EnqueuePrompt(ctx, database, chatID, kritui_db.InboxItem{
		SubmissionID: "submission-parked-queue",
		Delivery:     "queue",
		Model:        model,
		Message:      llm.Message{Role: "user", Content: "Parked queued prompt"},
	})
	if err != nil {
		t.Fatalf("enqueue parked prompt: %v", err)
	}

	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, _ *http.Request, w http.ResponseWriter) {
		if index != 0 {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		inboxTestWriteFinal(w, "Answer from parked queue")
	})
	inboxTestUseProvider(t, provider, model)
	toolCalls := newToolCallStore()
	mutation := inboxMutationHandler(database, registry, toolCalls, nil)

	target := "/chats/" + strconv.FormatInt(chatID, 10) + "/inbox/" + strconv.FormatInt(item.ID, 10)
	request := httptest.NewRequest(http.MethodPatch, target, strings.NewReader(url.Values{"delivery": {"steer"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetPathValue("chat", strconv.FormatInt(chatID, 10))
	request.SetPathValue("input", strconv.FormatInt(item.ID, 10))
	response := httptest.NewRecorder()
	mutation.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("idle PATCH status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if target := response.Header().Get("HX-Retarget"); target != "#messages" {
		t.Errorf("idle PATCH HX-Retarget = %q, want #messages", target)
	}
	if swap := response.Header().Get("HX-Reswap"); swap != "beforeend" {
		t.Errorf("idle PATCH HX-Reswap = %q, want beforeend", swap)
	}
	responseBody := response.Body.String()
	requireContains(t, responseBody,
		`id="prompt-inbox"`,
		"Parked queued prompt",
		`id="completion-`,
	)
	requestID := inboxTestRequestID(t, responseBody)

	provider.waitForRequest(t, 1)
	terminal := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, terminal, "Answer from parked queue")

	if count := provider.requestCount(); count != 1 {
		t.Fatalf("provider request count for idle PATCH = %d, want 1", count)
	}
	messages := inboxTestMessages(t, ctx, database, chatID)
	inboxTestRequireMessages(t, messages, []string{
		"assistant:Prior answer",
		"user:Parked queued prompt",
		"assistant:Answer from parked queue",
	})
	if count := inboxTestCountUser(messages, "Parked queued prompt"); count != 1 {
		t.Fatalf("stored parked prompt count = %d, want 1", count)
	}
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after idle PATCH completion = %#v, want empty", pending)
	}
	state, err := kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get completion state after idle PATCH: %v", err)
	}
	if state.Pending {
		t.Fatal("completion state is still pending after idle PATCH completion")
	}
	if active, ok := toolCalls.activeForUser(kritui_db.LegacyOwnerID, chatID); ok {
		t.Fatalf("idle PATCH left an active worker: %#v", active)
	}
}

func TestInboxMutationConflictKeepsPromptInboxCheckpoint(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const model = "inbox-model"
	chatID := seedChat(t, database, "Inbox stale mutation", nil, nil)
	mutation := inboxMutationHandler(database, inboxTestRegistry(t), newToolCallStore(), nil)

	enqueue := func(submission, content string) kritui_db.InboxItem {
		t.Helper()
		item, err := kritui_db.EnqueuePrompt(ctx, database, chatID, kritui_db.InboxItem{
			SubmissionID: submission,
			Delivery:     "queue",
			Model:        model,
			Message:      llm.Message{Role: "user", Content: content},
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", submission, err)
		}
		return item
	}
	cancelled := enqueue("stale-delete", "Stale delete prompt")
	delivered := enqueue("stale-patch", "Stale patch prompt")
	survivor := enqueue("surviving", "Surviving pending prompt")

	if err := kritui_db.CancelInboxPrompt(ctx, database, chatID, cancelled.ID); err != nil {
		t.Fatalf("cancel stale delete target: %v", err)
	}
	snapshot, err := kritui_db.GetMessageSnapshot(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get snapshot before stale target delivery: %v", err)
	}
	checkpoint, err := kritui_db.CheckpointCompletion(ctx, database, chatID, snapshot, nil, "input")
	if err != nil {
		t.Fatalf("deliver stale patch target: %v", err)
	}
	if len(checkpoint.Delivered) != 1 || checkpoint.Delivered[0].ID != delivered.ID {
		t.Fatalf("delivered stale target = %#v, want inbox item %d", checkpoint.Delivered, delivered.ID)
	}
	inboxTestPending(t,
		inboxTestInbox(t, ctx, database, chatID),
		[]string{"surviving"},
		[]string{"queue"},
	)

	staleDelete := inboxTestMutationRequest(t, mutation, http.MethodDelete, chatID, cancelled.ID, nil)
	inboxTestRequireConflictCheckpoint(t, staleDelete, survivor.ID, "Surviving pending prompt")
	stalePatch := inboxTestMutationRequest(t, mutation, http.MethodPatch, chatID, delivered.ID, url.Values{"delivery": {"queue"}})
	inboxTestRequireConflictCheckpoint(t, stalePatch, survivor.ID, "Surviving pending prompt")

	successfulDelete := inboxTestMutationRequest(t, mutation, http.MethodDelete, chatID, survivor.ID, nil)
	if successfulDelete.Code != http.StatusOK {
		t.Fatalf("successful mutation after stale conflicts status = %d, want %d; body = %q", successfulDelete.Code, http.StatusOK, successfulDelete.Body.String())
	}
	if retarget := successfulDelete.Header().Get("HX-Retarget"); retarget != "" {
		t.Errorf("successful mutation HX-Retarget = %q, want empty for direct prompt-inbox replacement", retarget)
	}
	successBody := successfulDelete.Body.String()
	requireContains(t, successBody, `id="prompt-inbox"`)
	requireNotContains(t, successBody, `role="alert"`, "<hx-partial")
	if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
		t.Fatalf("pending inbox after successful cleanup = %#v, want empty", pending)
	}
}

func TestInboxEnqueueAndFinishRaceKeepsOneActiveWorker(t *testing.T) {
	database := openTestDatabase(t)
	const model = "inbox-model"

	for iteration := 0; iteration < 8; iteration++ {
		iteration := iteration
		t.Run(fmt.Sprintf("iteration-%d", iteration+1), func(t *testing.T) {
			ctx := context.Background()
			chatID := seedChat(t, database, fmt.Sprintf("Inbox race %d", iteration+1), nil, nil)
			registry := inboxTestRegistry(t)

			firstRequest := newInboxTestGate()
			defer firstRequest.releaseNow()
			provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
				switch index {
				case 0:
					firstRequest.wait(r)
					inboxTestWriteFinal(w, "Raced first answer")
				case 1:
					inboxTestWriteFinal(w, "Raced queued answer")
				default:
					http.Error(w, "unexpected provider request", http.StatusInternalServerError)
				}
			})
			inboxTestUseProvider(t, provider, model)

			toolCalls := newToolCallStore()
			message := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)
			completion := messageCompletionHandler(database, registry, toolCalls, nil)

			requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
				"message":    {"Raced first prompt"},
				"model":      {model},
				"submission": {fmt.Sprintf("race-first-%d", iteration)},
			})
			inboxTestStartCompletion(t, completion, chatID, requestID, model)
			provider.waitForRequest(t, 1)

			enqueueResponse := make(chan *httptest.ResponseRecorder, 1)
			startEnqueue := make(chan struct{})
			go func() {
				<-startEnqueue
				enqueueResponse <- postForm(t, message, inboxTestChatTarget(chatID), url.Values{
					"message":    {"Raced queued prompt"},
					"model":      {model},
					"delivery":   {"queue"},
					"submission": {fmt.Sprintf("race-queued-%d", iteration)},
				})
			}()
			close(startEnqueue)
			firstRequest.releaseNow()

			waitForCompletionResult(t, toolCalls, requestID)

			var queued *httptest.ResponseRecorder
			select {
			case queued = <-enqueueResponse:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the concurrent enqueue response")
			}
			if queued.Code != http.StatusOK {
				t.Fatalf("concurrent enqueue status = %d, want %d; body = %q", queued.Code, http.StatusOK, queued.Body.String())
			}
			if inboxTestHasRequestID(queued.Body.String()) {
				queuedRequestID := inboxTestRequestID(t, queued.Body.String())
				inboxTestStartCompletion(t, completion, chatID, queuedRequestID, model)
				waitForCompletionResult(t, toolCalls, queuedRequestID)
			}

			if pending := inboxTestInbox(t, ctx, database, chatID); len(pending) != 0 {
				t.Fatalf("enqueue/finish race stranded pending inbox rows: %#v", pending)
			}
			messages := inboxTestMessages(t, ctx, database, chatID)
			if count := inboxTestCountUser(messages, "Raced queued prompt"); count != 1 {
				t.Fatalf("stored raced queued prompt count = %d, want 1; messages = %#v", count, inboxTestSignatures(messages))
			}
			if count := provider.requestCount(); count != 2 {
				t.Fatalf("provider request count = %d, want 2; messages = %#v", count, inboxTestSignatures(messages))
			}
			if active, ok := toolCalls.activeForUser(kritui_db.LegacyOwnerID, chatID); ok {
				t.Fatalf("enqueue/finish race left an active worker: %#v", active)
			}
		})
	}
}
