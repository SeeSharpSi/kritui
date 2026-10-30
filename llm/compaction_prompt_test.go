package llm

import (
	"context"
	"strings"
	"testing"
)

func compactionSummaryHistory() []Message {
	return []Message{
		{Role: "user", Content: "older question"},
		{Role: "assistant", ToolCalls: []ToolCall{elisionToolCall("old-compaction", "old")}},
		{Role: "tool", ToolCallID: "old-compaction", Content: strings.Repeat("ORIGINAL-OLD-RESULT ", 20)},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "current question"},
	}
}

func compactionSummarySystem(t *testing.T, raw []byte) string {
	t.Helper()
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	elisionRequest(t, raw, &request)
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
		t.Fatalf("summary messages = %#v, want system and user", request.Messages)
	}
	return request.Messages[0].Content
}

func TestToolResultSummaryPromptBlankUsesDefaultOnWire(t *testing.T) {
	summary := newElisionProvider(t, elisionTextResponse("chat", "short summary"))
	main := newElisionProvider(t, elisionTextResponse("chat", "done"))
	conversation := elisionConversation(t, main.client(t, "/v1/chat/completions", "main"), nil, compactionSummaryHistory()...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary"}, summary.client(t, "/v1/chat/completions", "summary")); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	conversation.SetToolResultSummaryPrompt(" \t\n ")
	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	got := compactionSummarySystem(t, summary.requestsFor(t, 1)[0])
	if got != DefaultCompactionPrompt() {
		t.Errorf("summary system prompt = %q, want default %q", got, DefaultCompactionPrompt())
	}
}

func TestToolResultSummaryPromptCustomIsExactAndCacheFollowsPromptChanges(t *testing.T) {
	custom := "\n  Summarize with the account compaction prompt.  \n"
	changed := "Changed compaction prompt."
	summary := newElisionProvider(t,
		elisionTextResponse("chat", "first short summary"),
		elisionTextResponse("chat", "second short summary"),
	)
	main := newElisionProvider(t,
		elisionTextResponse("chat", "done"),
		elisionTextResponse("chat", "sent again"),
		elisionTextResponse("chat", "sent after prompt change"),
	)
	conversation := elisionConversation(t, main.client(t, "/v1/chat/completions", "main"), nil, compactionSummaryHistory()...)
	if err := conversation.SetToolResultElision(ToolResultElisionConfig{Mode: ToolResultElisionSummarize, SummaryModel: "summary"}, summary.client(t, "/v1/chat/completions", "summary")); err != nil {
		t.Fatalf("SetToolResultElision() error: %v", err)
	}
	conversation.SetToolResultSummaryPrompt(custom)

	if err := conversation.Complete(context.Background()); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	first := compactionSummarySystem(t, summary.requestsFor(t, 1)[0])
	if first != custom {
		t.Errorf("first summary system prompt = %q, want exact custom %q", first, custom)
	}

	conversation.SetToolResultSummaryPrompt(custom)
	if err := conversation.Send(context.Background(), "same prompt keeps cache"); err != nil {
		t.Fatalf("Send() with same prompt error: %v", err)
	}
	if len(summary.requests) != 0 {
		t.Fatalf("same prompt caused %d summary request(s), want cached summary", len(summary.requests))
	}

	conversation.SetToolResultSummaryPrompt(changed)
	if err := conversation.Send(context.Background(), "changed prompt invalidates cache"); err != nil {
		t.Fatalf("Send() after prompt change error: %v", err)
	}
	second := compactionSummarySystem(t, summary.requestsFor(t, 1)[0])
	if second != changed {
		t.Errorf("second summary system prompt = %q, want changed %q", second, changed)
	}
	if len(summary.requests) != 0 {
		t.Fatalf("changed prompt caused %d extra summary request(s), want one regeneration", len(summary.requests))
	}
	main.requestsFor(t, 3)
}
