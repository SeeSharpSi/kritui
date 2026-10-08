package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"seesharpsi/kritui/tools"
)

type elisionProvider struct {
	server   *httptest.Server
	requests chan []byte
}

func newElisionProvider(t *testing.T, responses ...string) *elisionProvider {
	t.Helper()
	provider := &elisionProvider{requests: make(chan []byte, 32)}
	var requestCount atomic.Int32
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "could not read request", http.StatusBadRequest)
			return
		}
		provider.requests <- body
		index := int(requestCount.Add(1)) - 1
		if index >= len(responses) {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[index])
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *elisionProvider) client(t *testing.T, suffix, model string) *Client {
	t.Helper()
	client, err := New("key", model, provider.server.URL+suffix)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return client
}

func (provider *elisionProvider) requestsFor(t *testing.T, count int) [][]byte {
	t.Helper()
	requests := make([][]byte, count)
	for index := range count {
		select {
		case requests[index] = <-provider.requests:
		case <-time.After(2 * time.Second):
			t.Fatalf("provider request %d was not received", index+1)
		}
	}
	return requests
}

func elisionConversation(t *testing.T, client *Client, registry *tools.Registry, messages ...Message) *Conversation {
	t.Helper()
	conversation, err := NewConversation(client, registry, PromptContext{CurrentTime: time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)}, messages...)
	if err != nil {
		t.Fatalf("NewConversation() error: %v", err)
	}
	return conversation
}

func elisionToolCall(id, value string) ToolCall {
	arguments, _ := json.Marshal(map[string]string{"value": value})
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "lookup", Arguments: string(arguments)}}
}

func elisionTextResponse(protocol, content string) string {
	var body any
	switch protocol {
	case "chat":
		body = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}}
	case "messages":
		body = map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": "end_turn"}
	case "responses":
		body = map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}}}}
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func elisionToolResponse(calls ...ToolCall) string {
	encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
	return string(encoded)
}

func elisionRequest(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode provider request: %v\n%s", err, raw)
	}
}

func TestToolResultElisionNoneSendsOriginalAndKeepsCanonicalHistory(t *testing.T) {
	for _, test := range []struct {
		name    string
		setMode bool
	}{
		{name: "zero value"},
		{name: "explicit none", setMode: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newElisionProvider(t, elisionTextResponse("chat", "done"))
			conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), nil, elisionCrossProtocolHistory(t)...)
			if test.setMode {
				if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionNone}, nil); err != nil {
					t.Fatalf("SetToolResultElision() error: %v", err)
				}
			}
			before := conversation.Messages()
			if err := conversation.Complete(context.Background()); err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			request := provider.requestsFor(t, 1)[0]
			got := chatToolResults(t, request)
			if got["old-a"] != strings.Repeat("old-A", 20) || got["old-b"] != strings.Repeat("old-B", 20) {
				t.Errorf("old tool outputs = %#v, want original contents", got)
			}
			if got["fresh-a"] != strings.Repeat("fresh-A", 20) || got["fresh-b"] != strings.Repeat("fresh-B", 20) {
				t.Errorf("fresh tool outputs = %#v, want original contents", got)
			}
			after := conversation.Messages()
			if !reflect.DeepEqual(after[:len(before)], before) {
				t.Error("provider request or completion changed canonical history")
			}
		})
	}
}

func TestToolResultElisionUserTurnCutoffs(t *testing.T) {
	turnHistory := []Message{
		{Role: "user", Content: "user one"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("turn-1", "one")}},
		{Role: "tool", ToolCallID: "turn-1", Content: "result-one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "user two"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("turn-2", "two")}},
		{Role: "tool", ToolCallID: "turn-2", Content: "result-two"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Content: "user three"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("turn-3", "three")}},
		{Role: "tool", ToolCallID: "turn-3", Content: "result-three"},
		{Role: "assistant", Content: "answer three"},
	}
	for _, test := range []struct {
		name string
		mode ToolResultElisionConfig
		want map[string]string
	}{
		{
			name: "current turn cuts earlier user messages",
			mode: ToolResultElisionConfig{Mode: ToolResultElisionCurrentTurn},
			want: map[string]string{"turn-1": elidedToolResultMarker, "turn-2": elidedToolResultMarker, "turn-3": "result-three"},
		},
		{
			name: "last two user turns keep exactly latest two",
			mode: ToolResultElisionConfig{Mode: ToolResultElisionLastUserTurns, UserTurns: 2},
			want: map[string]string{"turn-1": elidedToolResultMarker, "turn-2": "result-two", "turn-3": "result-three"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newElisionProvider(t, elisionTextResponse("chat", "done"))
			conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), nil, turnHistory...)
			if err := conversation.SetToolResultElision(test.mode, nil); err != nil {
				t.Fatalf("SetToolResultElision() error: %v", err)
			}
			if err := conversation.Complete(context.Background()); err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			got := chatToolResults(t, provider.requestsFor(t, 1)[0])
			for id, want := range test.want {
				if got[id] != want {
					t.Errorf("tool result %q = %q, want %q", id, got[id], want)
				}
			}
		})
	}
}

func TestToolResultElisionLastUserTurnsRequiresActualUserMessages(t *testing.T) {
	for _, test := range []struct {
		name    string
		history []Message
		turns   int
	}{
		{
			name:  "exactly X turns preserves all results",
			turns: 2,
			history: []Message{
				{Role: "user", Content: "first"},
				{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("exact-1", "1")}},
				{Role: "tool", ToolCallID: "exact-1", Content: "first-result"},
				{Role: "assistant", Content: "first answer"},
				{Role: "user", Content: "second"},
				{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("exact-2", "2")}},
				{Role: "tool", ToolCallID: "exact-2", Content: "second-result"},
				{Role: "assistant", Content: "second answer"},
			},
		},
		{
			name:  "fewer than X actual turns does not count Messages tool-result user blocks",
			turns: 3,
			history: []Message{
				{Role: "user", Content: "only actual user"},
				{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("messages-1", "1")}},
				{Role: "tool", ToolCallID: "messages-1", Content: "tool result one"},
				{Role: "assistant", Content: "middle answer"},
				{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("messages-2", "2")}},
				{Role: "tool", ToolCallID: "messages-2", Content: "tool result two"},
				{Role: "assistant", Content: "last answer"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newElisionProvider(t, elisionTextResponse("messages", "done"))
			conversation := elisionConversation(t, provider.client(t, "/v1/chat/messages", "main"), nil, test.history...)
			if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionLastUserTurns, UserTurns: test.turns}, nil); err != nil {
				t.Fatalf("SetToolResultElision() error: %v", err)
			}
			if err := conversation.Complete(context.Background()); err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			got := messagesToolResults(t, provider.requestsFor(t, 1)[0])
			for _, message := range test.history {
				if message.Role == "tool" && got[message.ToolCallID] != message.Content {
					t.Errorf("Messages tool result %q = %q, want unchanged %q", message.ToolCallID, got[message.ToolCallID], message.Content)
				}
			}
		})
	}
}

func TestToolResultElisionBudgetPreservesFreshBatchAndAccountsForMarkers(t *testing.T) {
	oldCalls := []ToolCall{elisionToolCall("budget-old", "old"), elisionToolCall("budget-short", "short"), elisionToolCall("budget-newer", "newer")}
	history := []Message{
		{Role: "user", Content: "question"},
		{Role: "assistant", ToolCalls: oldCalls},
		{Role: "tool", ToolCallID: "budget-old", Content: strings.Repeat("O", 24)},
		{Role: "tool", ToolCallID: "budget-short", Content: "tiny"},
		{Role: "tool", ToolCallID: "budget-newer", Content: strings.Repeat("N", 24)},
		{Role: "assistant", Content: "continuing"},
		{Role: "user", Content: "latest"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("budget-fresh-a", "fresh-a"), elisionToolCall("budget-fresh-b", "fresh-b")}},
		{Role: "tool", ToolCallID: "budget-fresh-a", Content: "fresh-a"},
		{Role: "tool", ToolCallID: "budget-fresh-b", Content: "fresh-b"},
	}
	provider := newElisionProvider(t, elisionTextResponse("chat", "done"))
	conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), nil, history...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionBudget, TokenBudget: 15}, nil); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	got := chatToolResults(t, provider.requestsFor(t, 1)[0])
	want := map[string]string{
		"budget-old":     elidedToolResultMarker,
		"budget-short":   "tiny",
		"budget-newer":   elidedToolResultMarker,
		"budget-fresh-a": "fresh-a",
		"budget-fresh-b": "fresh-b",
	}
	for id, value := range want {
		if got[id] != value {
			t.Errorf("tool result %q = %q, want %q", id, got[id], value)
		}
	}
	// The two markers each cost five tokens. Counting only removed source text
	// would stop after budget-old; marker-inclusive accounting must also elide
	// budget-newer to reach the configured 15-token floor.
	if got["budget-old"] != elidedToolResultMarker || got["budget-newer"] != elidedToolResultMarker {
		t.Errorf("budget did not account for marker cost: %#v", got)
	}
}

func TestToolResultElisionBudgetReducesOldestConsumedResultFirst(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("oldest", "old"), elisionToolCall("newer", "new"), elisionToolCall("short", "short")}},
		{Role: "tool", ToolCallID: "oldest", Content: strings.Repeat("O", 40)},
		{Role: "tool", ToolCallID: "newer", Content: strings.Repeat("N", 12)},
		{Role: "tool", ToolCallID: "short", Content: "tiny"},
		{Role: "assistant", Content: "continued"},
		{Role: "user", Content: "latest"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("fresh", "fresh")}},
		{Role: "tool", ToolCallID: "fresh", Content: "newest"},
	}
	provider := newElisionProvider(t, elisionTextResponse("chat", "done"))
	conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), nil, history...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionBudget, TokenBudget: 11}, nil); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	got := chatToolResults(t, provider.requestsFor(t, 1)[0])
	want := map[string]string{"oldest": elidedToolResultMarker, "newer": strings.Repeat("N", 12), "short": "tiny", "fresh": "newest"}
	assertToolResults(t, got, want)
}

func TestToolResultElisionBudgetKeepsFreshBatchEvenOverBudget(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("fresh-a", "a"), elisionToolCall("fresh-b", "b")}},
		{Role: "tool", ToolCallID: "fresh-a", Content: strings.Repeat("A", 80)},
		{Role: "tool", ToolCallID: "fresh-b", Content: strings.Repeat("B", 80)},
	}
	provider := newElisionProvider(t, elisionTextResponse("chat", "done"))
	conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), nil, history...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionBudget, TokenBudget: 1}, nil); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	got := chatToolResults(t, provider.requestsFor(t, 1)[0])
	if got["fresh-a"] != strings.Repeat("A", 80) || got["fresh-b"] != strings.Repeat("B", 80) {
		t.Errorf("fresh tool batch was reduced over budget: %#v", got)
	}
}

func TestToolResultElisionMakesPreviousFreshBatchEligibleOnLaterToolRound(t *testing.T) {
	first := strings.Repeat("first-result-", 8)
	second := strings.Repeat("second-result-", 8)
	provider := newElisionProvider(t,
		elisionToolResponse(elisionToolCall("round-1", first)),
		elisionToolResponse(elisionToolCall("round-2", second)),
		elisionTextResponse("chat", "done"),
	)
	registry, err := tools.NewRegistry(elisionValueTool{})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	conversation := elisionConversation(t, provider.client(t, "/v1/chat/completions", "main"), registry)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionBudget, TokenBudget: 1}, nil); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	requests := provider.requestsFor(t, 3)
	secondRound := chatToolResults(t, requests[1])
	if secondRound["round-1"] != first {
		t.Errorf("fresh first-round output = %q, want full result", secondRound["round-1"])
	}
	thirdRound := chatToolResults(t, requests[2])
	if thirdRound["round-1"] != elidedToolResultMarker || thirdRound["round-2"] != second {
		t.Errorf("later-round outputs = %#v, want old result elided and newest batch intact", thirdRound)
	}
	messages := conversation.Messages()
	if len(messages) != 7 || messages[2].Role != "assistant" || messages[3].Role != "tool" || messages[3].ToolCallID != "round-1" || messages[4].Role != "assistant" || messages[5].Role != "tool" || messages[5].ToolCallID != "round-2" {
		t.Errorf("canonical tool IDs or role order changed: %#v", messages)
	}
}

func TestToolResultElisionCrossProtocolPreservesIDsMetadataAndCanonicalHistory(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
	}{
		{name: "Chat Completions", suffix: "/v1/chat/completions"},
		{name: "Messages", suffix: "/v1/chat/messages"},
		{name: "Responses", suffix: "/v1/responses"},
	} {
		t.Run(test.name, func(t *testing.T) {
			protocol := "chat"
			if strings.Contains(test.name, "Messages") {
				protocol = "messages"
			} else if strings.Contains(test.name, "Responses") {
				protocol = "responses"
			}
			provider := newElisionProvider(t, elisionTextResponse(protocol, "done"))
			conversation := elisionConversation(t, provider.client(t, test.suffix, "main"), nil, elisionCrossProtocolHistory(t)...)
			if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionCurrentTurn}, nil); err != nil {
				t.Fatalf("SetToolResultElision() error: %v", err)
			}
			before := conversation.Messages()
			if protocol == "responses" {
				copy := before[2].ProviderMetadata.ResponsesOutput()
				copy[0][0] = 'x'
				if got := conversation.Messages()[2].ProviderMetadata.ResponsesOutput()[0]; got[0] != '{' {
					t.Fatal("Messages() metadata copy shares raw Responses output memory")
				}
			}
			if err := conversation.Complete(context.Background()); err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			request := provider.requestsFor(t, 1)[0]
			switch protocol {
			case "chat":
				assertToolResults(t, chatToolResults(t, request), elisionExpectedResults())
				assertChatCallIDs(t, request, []string{"old-a", "old-b", "fresh-a", "fresh-b"})
			case "messages":
				assertToolResults(t, messagesToolResults(t, request), elisionExpectedResults())
				assertMessagesCallIDs(t, request, []string{"old-a", "old-b", "fresh-a", "fresh-b"})
			case "responses":
				assertToolResults(t, responsesToolResults(t, request), elisionExpectedResults())
				assertResponsesMetadata(t, request)
			}
			after := conversation.Messages()
			if !reflect.DeepEqual(after[:len(before)], before) {
				t.Error("elision changed canonical roles, call IDs, results, or provider metadata")
			}
			if len(after) != len(before)+1 || after[len(after)-1].Role != "assistant" || after[len(after)-1].Content != "done" {
				t.Errorf("canonical completion tail = %#v, want one final assistant answer", after[len(before):])
			}
		})
	}
}

func TestToolResultElisionSummarizesOnlyOlderResultsAndCachesAcrossRoundsAndSend(t *testing.T) {
	oldResult := strings.Repeat("old fact ", 16)
	freshResult := strings.Repeat("fresh fact ", 16)
	main := newElisionProvider(t,
		elisionToolResponse(elisionToolCall("main-round-1", strings.Repeat("round one ", 10))),
		elisionToolResponse(elisionToolCall("main-round-2", strings.Repeat("round two ", 10))),
		elisionTextResponse("chat", "finished"),
		elisionTextResponse("chat", "sent again"),
	)
	summary := newElisionProvider(t,
		elisionTextResponse("chat", "short summary"),
		elisionTextResponse("chat", "short summary"),
		elisionTextResponse("chat", "short summary"),
		elisionTextResponse("chat", "short summary"),
	)
	registry, err := tools.NewRegistry(elisionValueTool{})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	history := []Message{
		{Role: "user", Content: "older question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("old-summary", "old")}},
		{Role: "tool", ToolCallID: "old-summary", Content: oldResult},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "current question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("fresh-summary", "fresh")}},
		{Role: "tool", ToolCallID: "fresh-summary", Content: freshResult},
	}
	conversation := elisionConversation(t, main.client(t, "/v1/chat/completions", "main"), registry, history...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary-model"}, summary.client(t, "/v1/chat/completions", "summary-model")); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	mainRequests := main.requestsFor(t, 3)
	for index, raw := range mainRequests {
		var request struct {
			Model string `json:"model"`
		}
		elisionRequest(t, raw, &request)
		if request.Model != "main" {
			t.Errorf("main request %d model = %q, want separate main model", index+1, request.Model)
		}
	}
	first := chatToolResults(t, mainRequests[0])
	if first["old-summary"] != summarizedToolResultMarker+"short summary" || first["fresh-summary"] != freshResult {
		t.Errorf("first main round results = %#v, want old summary and fresh full result", first)
	}
	second := chatToolResults(t, mainRequests[1])
	if second["old-summary"] != summarizedToolResultMarker+"short summary" || second["fresh-summary"] != freshResult {
		t.Errorf("second main round results = %#v, want cached old summary and fresh current-turn result", second)
	}
	if err := conversation.Send(context.Background(), "next user turn"); err != nil {
		t.Fatalf("Send() after completion error: %v", err)
	}
	lastMainRequest := main.requestsFor(t, 1)[0]
	lastResults := chatToolResults(t, lastMainRequest)
	for _, id := range []string{"old-summary", "fresh-summary", "main-round-1", "main-round-2"} {
		if lastResults[id] != summarizedToolResultMarker+"short summary" {
			t.Errorf("post-Send tool result %q = %q, want cached or fresh summary", id, lastResults[id])
		}
	}
	summaryRequests := summary.requestsFor(t, 4)
	// The old result was summarized once before three main-provider rounds. Its
	// cached summary is reused; Send makes the three previously fresh results
	// eligible and requires one summary apiece.
	if len(summaryRequests) != 4 {
		t.Fatalf("summary request count after all main rounds and Send = %d, want one old result and three newly eligible results", len(summaryRequests))
	}
	wantPrompts := []string{oldResult, freshResult, strings.Repeat("round one ", 10), strings.Repeat("round two ", 10)}
	for index, raw := range summaryRequests {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		elisionRequest(t, raw, &request)
		if request.Model != "summary-model" || len(request.Messages) != 2 || len(request.Tools) != 0 {
			t.Errorf("summary request %d model/messages/tools = %q/%d/%d, want isolated two-message summary request without tools", index+1, request.Model, len(request.Messages), len(request.Tools))
			continue
		}
		wantResult := wantPrompts[index]
		if request.Messages[0].Role != "system" || request.Messages[1].Role != "user" || !strings.Contains(request.Messages[1].Content, "Tool function: lookup") || !strings.Contains(request.Messages[1].Content, "Tool arguments:") || !strings.Contains(request.Messages[1].Content, wantResult) {
			t.Errorf("summary prompt %d = %#v, want isolated context for result %q", index+1, request.Messages, wantResult)
		}
	}
	if len(summary.requests) != 0 {
		t.Errorf("summary provider received %d unexpected duplicate request(s)", len(summary.requests))
	}
}

func TestToolResultElisionOversizedSummaryKeepsOriginal(t *testing.T) {
	original := "keep this original result"
	main := newElisionProvider(t, elisionTextResponse("chat", "done"))
	summaryText := strings.Repeat("much longer summary ", 8)
	summary := newElisionProvider(t, elisionTextResponse("chat", summaryText))
	history := []Message{
		{Role: "user", Content: "older"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("oversized", "x")}},
		{Role: "tool", ToolCallID: "oversized", Content: original},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "current"},
	}
	conversation := elisionConversation(t, main.client(t, "/v1/chat/completions", "main"), nil, history...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary"}, summary.client(t, "/v1/chat/completions", "summary")); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	got := chatToolResults(t, main.requestsFor(t, 1)[0])
	if got["oversized"] != original {
		t.Errorf("oversized summary replaced original: got %q, want %q", got["oversized"], original)
	}
	_ = summary.requestsFor(t, 1)
}

func TestToolResultElisionSummaryErrorsAndCancellationDoNotMutateHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		cancel     bool
		wantCancel bool
	}{
		{name: "provider error"},
		{name: "context cancellation", cancel: true, wantCancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var summary *elisionProvider
			if test.cancel {
				summary = newElisionProvider(t, elisionTextResponse("chat", "unused"))
			} else {
				summary = &elisionProvider{requests: make(chan []byte, 32)}
				summary.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					summary.requests <- body
					http.Error(w, `{"error":{"message":"summary unavailable"}}`, http.StatusServiceUnavailable)
				}))
				t.Cleanup(summary.server.Close)
			}
			main := newElisionProvider(t, elisionTextResponse("chat", "done"))
			history := []Message{
				{Role: "user", Content: "older"},
				{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("error-result", "x")}},
				{Role: "tool", ToolCallID: "error-result", Content: strings.Repeat("original ", 8)},
				{Role: "assistant", Content: "older answer"},
				{Role: "user", Content: "current"},
			}
			conversation := elisionConversation(t, main.client(t, "/v1/chat/completions", "main"), nil, history...)
			if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary"}, summary.client(t, "/v1/chat/completions", "summary")); err != nil {
				t.Fatalf("SetToolResultElision() error: %v", err)
			}
			before := conversation.Messages()
			ctx := context.Background()
			cancel := func() {}
			if test.cancel {
				var cancelContext context.CancelFunc
				ctx, cancelContext = context.WithCancel(ctx)
				cancelContext()
				cancel = cancelContext
			}
			defer cancel()
			err := conversation.Complete(ctx)
			if test.wantCancel {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Complete() error = %v, want context.Canceled", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "503") {
				t.Errorf("Complete() error = %v, want summarizer provider error", err)
			}
			if after := conversation.Messages(); !reflect.DeepEqual(after, before) {
				t.Error("summary error or cancellation mutated canonical history")
			}
			if len(main.requests) != 0 {
				t.Error("main provider was called after summary failure")
			}
		})
	}
}

func TestToolResultElisionConfigurationErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		config ToolResultElisionConfig
		want   string
	}{
		{name: "unsupported mode", config: ToolResultElisionConfig{Mode: "unknown"}, want: "unsupported tool-result elision mode"},
		{name: "zero user turns", config: ToolResultElisionConfig{Mode: ToolResultElisionLastUserTurns}, want: "between 1 and 1000"},
		{name: "user turn upper bound", config: ToolResultElisionConfig{Mode: ToolResultElisionLastUserTurns, UserTurns: MaxElisionUserTurns + 1}, want: "between 1 and 1000"},
		{name: "zero token budget", config: ToolResultElisionConfig{Mode: ToolResultElisionBudget}, want: "between 1 and 10000000"},
		{name: "token budget upper bound", config: ToolResultElisionConfig{Mode: ToolResultElisionBudget, TokenBudget: MaxElisionTokenBudget + 1}, want: "between 1 and 10000000"},
		{name: "missing summary model", config: ToolResultElisionConfig{Mode: ToolResultElisionSummarize}, want: "summary model is required"},
		{name: "summary model length", config: ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: strings.Repeat("m", 513)}, want: "at most 512 bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.config.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want message containing %q", err, test.want)
			}
		})
	}
	client, err := New("key", "main", "https://example.test/v1/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	conversation := elisionConversation(t, client, nil)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary"}, nil); err == nil || !strings.Contains(err.Error(), "summarizer client is required") {
		t.Errorf("SetToolResultElision() missing summarizer error = %v", err)
	}
}

func elisionCrossProtocolHistory(t *testing.T) []Message {
	t.Helper()
	oldMetadata := elisionResponsesMetadata(t,
		`{"type":"reasoning","id":"reason-old","encrypted_content":"opaque-old"}`,
		`{"type":"function_call","call_id":"old-a","name":"lookup","arguments":"{\"value\":\"old-a\"}"}`,
		`{"type":"function_call","call_id":"old-b","name":"lookup","arguments":"{\"value\":\"old-b\"}"}`,
	)
	freshMetadata := elisionResponsesMetadata(t,
		`{"type":"reasoning","id":"reason-fresh","encrypted_content":"opaque-fresh"}`,
		`{"type":"function_call","call_id":"fresh-a","name":"lookup","arguments":"{\"value\":\"fresh-a\"}"}`,
		`{"type":"function_call","call_id":"fresh-b","name":"lookup","arguments":"{\"value\":\"fresh-b\"}"}`,
	)
	return []Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("old-a", "old-a"), elisionToolCall("old-b", "old-b")}, ProviderMetadata: oldMetadata},
		{Role: "tool", ToolCallID: "old-a", Content: strings.Repeat("old-A", 20)},
		{Role: "tool", ToolCallID: "old-b", Content: strings.Repeat("old-B", 20)},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "fresh question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("fresh-a", "fresh-a"), elisionToolCall("fresh-b", "fresh-b")}, ProviderMetadata: freshMetadata},
		{Role: "tool", ToolCallID: "fresh-a", Content: strings.Repeat("fresh-A", 20)},
		{Role: "tool", ToolCallID: "fresh-b", Content: strings.Repeat("fresh-B", 20)},
	}
}

func elisionResponsesMetadata(t *testing.T, output ...string) ProviderMetadata {
	t.Helper()
	raw := make([]json.RawMessage, len(output))
	for index := range output {
		raw[index] = json.RawMessage(output[index])
	}
	metadata, err := NewResponsesProviderMetadata(raw)
	if err != nil {
		t.Fatalf("NewResponsesProviderMetadata() error: %v", err)
	}
	return metadata
}

func elisionExpectedResults() map[string]string {
	return map[string]string{
		"old-a":   elidedToolResultMarker,
		"old-b":   elidedToolResultMarker,
		"fresh-a": strings.Repeat("fresh-A", 20),
		"fresh-b": strings.Repeat("fresh-B", 20),
	}
}

func assertToolResults(t *testing.T, got, want map[string]string) {
	t.Helper()
	for id, content := range want {
		if got[id] != content {
			t.Errorf("tool result %q = %q, want %q", id, got[id], content)
		}
	}
}

func chatToolResults(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var request struct {
		Messages []struct {
			Role       string     `json:"role"`
			Content    string     `json:"content"`
			ToolCallID string     `json:"tool_call_id"`
			ToolCalls  []ToolCall `json:"tool_calls"`
		} `json:"messages"`
	}
	elisionRequest(t, raw, &request)
	results := make(map[string]string)
	for _, message := range request.Messages {
		if message.Role == "tool" {
			results[message.ToolCallID] = message.Content
		}
	}
	return results
}

func messagesToolResults(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var request struct {
		Messages []messagesMessage `json:"messages"`
	}
	elisionRequest(t, raw, &request)
	results := make(map[string]string)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				results[block.ToolUseID] = block.Content
			}
		}
	}
	return results
}

func responsesToolResults(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var request struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		} `json:"input"`
	}
	elisionRequest(t, raw, &request)
	results := make(map[string]string)
	for _, item := range request.Input {
		if item.Type == "function_call_output" {
			results[item.CallID] = item.Output
		}
	}
	return results
}

func assertChatCallIDs(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var request struct {
		Messages []struct {
			Role      string     `json:"role"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"messages"`
	}
	elisionRequest(t, raw, &request)
	var got []string
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				got = append(got, call.ID)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Chat Completions call IDs = %v, want %v", got, want)
	}
}

func assertMessagesCallIDs(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var request struct {
		Messages []messagesMessage `json:"messages"`
	}
	elisionRequest(t, raw, &request)
	var got []string
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" {
				got = append(got, block.ID)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Messages tool-use IDs = %v, want %v", got, want)
	}
}

func assertResponsesMetadata(t *testing.T, raw []byte) {
	t.Helper()
	wire := string(raw)
	for _, want := range []string{`"type":"reasoning"`, `"type":"function_call"`, "opaque-old", "opaque-fresh", "reason-old", "reason-fresh", `"call_id":"old-a"`, `"call_id":"old-b"`, `"call_id":"fresh-a"`, `"call_id":"fresh-b"`} {
		if !strings.Contains(wire, want) {
			t.Errorf("Responses request lost provider metadata or call ID %q: %s", want, wire)
		}
	}
}

type elisionValueTool struct{}

func (elisionValueTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "lookup",
		Description: "Returns configured result text",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
	}
}

func (elisionValueTool) Execute(_ context.Context, arguments json.RawMessage) (string, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	return input.Value, nil
}
