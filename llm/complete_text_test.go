package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompleteTextSendsExactlySystemAndUser(t *testing.T) {
	var captured struct {
		Model    string        `json:"model"`
		Messages []Message     `json:"messages"`
		Tools    []interface{} `json:"tools"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"model","choices":[{"message":{"role":"assistant","content":"Generated title"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	title, err := client.CompleteText(context.Background(), "System prompt", "First user message")
	if err != nil {
		t.Fatalf("CompleteText() error: %v", err)
	}
	if title != "Generated title" {
		t.Fatalf("CompleteText() = %q, want Generated title", title)
	}
	if len(captured.Messages) != 2 {
		t.Fatalf("request messages = %#v, want exactly two", captured.Messages)
	}
	if captured.Messages[0].Role != "system" || captured.Messages[0].Content != "System prompt" {
		t.Errorf("system message = %#v, want exact prompt", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || captured.Messages[1].Content != "First user message" {
		t.Errorf("user message = %#v, want exact content", captured.Messages[1])
	}
	if len(captured.Tools) != 0 {
		t.Errorf("tools = %#v, want none", captured.Tools)
	}
}

func TestCompleteTextRejectsToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"model","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if _, err := client.CompleteText(context.Background(), "System prompt", "First user message"); err == nil {
		t.Fatal("CompleteText() error = nil, want tool-call rejection")
	}
}
