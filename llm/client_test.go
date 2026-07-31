package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRequiresConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		apiKey   string
		model    string
		endpoint string
	}{
		{name: "key", model: "model", endpoint: "https://example.com/v1/chat/completions"},
		{name: "model", apiKey: "key", endpoint: "https://example.com/v1/chat/completions"},
		{name: "endpoint", apiKey: "key", model: "model"},
		{name: "absolute endpoint", apiKey: "key", model: "model", endpoint: "/v1/chat/completions"},
		{name: "HTTP endpoint", apiKey: "key", model: "model", endpoint: "ftp://example.com/chat"},
		{name: "fragment-free endpoint", apiKey: "key", model: "model", endpoint: "https://example.com/chat#fragment"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.apiKey, test.model, test.endpoint); err == nil {
				t.Fatal("New() error = nil, want configuration error")
			}
		})
	}
}

func TestComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.RequestURI() != "/custom/chat?api-version=1" {
			t.Errorf("request URI = %q, want exact configured endpoint", r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("Authorization = %q, want Bearer secret-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		var request struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Model != "configured-model" {
			t.Errorf("model = %q, want configured-model", request.Model)
		}
		if len(request.Messages) != 1 || request.Messages[0].Role != "user" || request.Messages[0].Content != "Hello" {
			t.Errorf("messages = %#v, want user Hello message", request.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"completion-1",
			"model":"configured-model",
			"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
		}`))
	}))
	defer server.Close()

	client, err := New("secret-key", "configured-model", server.URL+"/custom/chat?api-version=1")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	completion, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "Hello"}})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if completion.ID != "completion-1" || completion.Model != "configured-model" {
		t.Errorf("completion metadata = %#v", completion)
	}
	if completion.Message.Role != "assistant" || completion.Message.Content != "Hi" {
		t.Errorf("message = %#v, want assistant Hi message", completion.Message)
	}
	if completion.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", completion.FinishReason)
	}
	if completion.Usage != (Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}) {
		t.Errorf("usage = %#v, want token counts 1, 2, 3", completion.Usage)
	}
}

func TestCompleteRequiresMessages(t *testing.T) {
	client, err := New("key", "model", "https://example.com/v1/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if _, err := client.Complete(context.Background(), nil); err == nil {
		t.Fatal("Complete() error = nil, want missing messages error")
	}
}

func TestModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.RequestURI() != "/v1/models?api-version=1" {
			t.Errorf("request URI = %q, want models endpoint", r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("Authorization = %q, want Bearer secret-key", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer server.Close()

	client, err := New("secret-key", "model-a", server.URL+"/v1/chat/completions?api-version=1")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Errorf("Models() = %#v, want model-a and model-b", models)
	}
}

func TestCompleteReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer server.Close()

	client, err := New("wrong-key", "model", server.URL)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "Hello"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "invalid key" {
		t.Errorf("APIError = %#v, want status 401 and invalid key", apiErr)
	}
	if !strings.Contains(apiErr.Body, "invalid key") {
		t.Errorf("APIError body = %q, want provider response", apiErr.Body)
	}
}

func TestCompleteRejectsResponseWithoutChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"completion-1","choices":[]}`))
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "Hello"}})
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("Complete() error = %v, want no choices error", err)
	}
}
