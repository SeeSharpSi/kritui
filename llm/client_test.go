package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"seesharpsi/kritui/tools"
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

func TestNewConfiguresProviderDeadlines(t *testing.T) {
	client, err := New("key", "model", "https://example.com/v1/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP transport = %T, want *http.Transport", client.httpClient.Transport)
	}
	if transport.DialContext == nil {
		t.Error("provider DialContext = nil, want connect deadline")
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Errorf("response header timeout = %s, want operation context deadline", transport.ResponseHeaderTimeout)
	}
	if client.modelsTimeout != defaultModelsTimeout {
		t.Errorf("models timeout = %s, want %s", client.modelsTimeout, defaultModelsTimeout)
	}
	if client.completeTimeout != defaultCompletionTimeout {
		t.Errorf("completion timeout = %s, want %s", client.completeTimeout, defaultCompletionTimeout)
	}
}

func TestProviderOperationsTimeOut(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(context.Context, *Client) error
	}{
		{
			name: "models",
			invoke: func(ctx context.Context, client *Client) error {
				_, err := client.Models(ctx)
				return err
			},
		},
		{
			name: "completion",
			invoke: func(ctx context.Context, client *Client) error {
				_, err := client.complete(ctx, []Message{{Role: "user", Content: "Hello"}}, nil)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				<-release
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL+"/v1/chat/completions", ClientOptions{
				ModelsTimeout:     100 * time.Millisecond,
				CompletionTimeout: 100 * time.Millisecond,
			})
			if err != nil {
				close(release)
				t.Fatalf("New() error: %v", err)
			}
			err = test.invoke(context.Background(), client)
			close(release)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("operation error = %v, want context deadline exceeded", err)
			}
		})
	}
}

func TestProviderOperationsRejectOversizedResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		invoke   func(*Client) error
	}{
		{
			name:     "models",
			response: `{"data":[{"id":"` + strings.Repeat("m", 256) + `"}]}`,
			invoke: func(client *Client) error {
				_, err := client.Models(context.Background())
				return err
			},
		},
		{
			name:     "completion",
			response: `{"choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("x", 256) + `"},"finish_reason":"stop"}]}`,
			invoke: func(client *Client) error {
				_, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL+"/v1/chat/completions", ClientOptions{MaxResponseBodySize: 128})
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			err = test.invoke(client)
			if err == nil || !strings.Contains(err.Error(), "response exceeds 128 bytes") {
				t.Fatalf("operation error = %v, want response-size error", err)
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

	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatalf("complete() error: %v", err)
	}
	if message.Role != "assistant" || message.Content != "Hi" {
		t.Errorf("message = %#v, want assistant Hi message", message)
	}
	if message.Model != "configured-model" {
		t.Errorf("model = %q, want configured-model", message.Model)
	}
	totalTokens := 3
	if message.TotalTokens == nil || *message.TotalTokens != totalTokens {
		t.Errorf("total tokens = %v, want %d", message.TotalTokens, totalTokens)
	}
	if message.Cost != nil {
		t.Errorf("cost = %v, want nil", *message.Cost)
	}
}

func TestCompleteRequiresMessages(t *testing.T) {
	client, err := New("key", "model", "https://example.com/v1/chat/completions")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if _, err := client.complete(context.Background(), nil, nil); err == nil {
		t.Fatal("Complete() error = nil, want missing messages error")
	}
}

func TestSystemPromptWithContext(t *testing.T) {
	currentTime := time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)
	clientLocation := time.FixedZone("America/New_York", -4*60*60)

	got := systemPromptWithContext(PromptContext{
		CurrentTime:    currentTime,
		ClientLocation: clientLocation,
	})
	want := strings.TrimSpace(systemPrompt) + `

## Current date and time
Current UTC datetime: 2026-08-02T18:30:00Z
Client datetime: 2026-08-02T14:30:00-04:00
Client timezone: America/New_York`
	if got != want {
		t.Errorf("systemPromptWithContext() = %q, want %q", got, want)
	}

	got = systemPromptWithContext(PromptContext{CurrentTime: currentTime})
	want = strings.TrimSpace(systemPrompt) + `

## Current date and time
Current UTC datetime: 2026-08-02T18:30:00Z
Client may be in different timezone; if giving times, specify that they're in UTC.`
	if got != want {
		t.Errorf("fallback systemPromptWithContext() = %q, want %q", got, want)
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

	_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
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

	_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("Complete() error = %v, want no choices error", err)
	}
}

func TestCompleteValidatesChatCompletionResponse(t *testing.T) {
	validCall := `{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}`
	tests := []struct {
		name      string
		choice    string
		wantError string
		wantCalls int
	}{
		{
			name:      "missing role",
			choice:    `{"message":{"content":"answer"},"finish_reason":"stop"}`,
			wantError: "role must be assistant",
		},
		{
			name:      "wrong role",
			choice:    `{"message":{"role":"user","content":"answer"},"finish_reason":"stop"}`,
			wantError: "role must be assistant",
		},
		{
			name:      "empty message",
			choice:    `{"message":{"role":"assistant","content":"  "},"finish_reason":"stop"}`,
			wantError: "must contain content or tool calls",
		},
		{
			name:      "unsupported finish reason",
			choice:    `{"message":{"role":"assistant","content":"answer"},"finish_reason":"unknown"}`,
			wantError: "unsupported finish reason",
		},
		{
			name:      "tool finish without calls",
			choice:    `{"message":{"role":"assistant"},"finish_reason":"tool_calls"}`,
			wantError: "contained no tool calls",
		},
		{
			name:      "stop with calls",
			choice:    `{"message":{"role":"assistant","tool_calls":[` + validCall + `]},"finish_reason":"stop"}`,
			wantError: "cannot contain tool calls",
		},
		{
			name:      "duplicate call IDs",
			choice:    `{"message":{"role":"assistant","tool_calls":[` + validCall + `,{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}`,
			wantError: "duplicate tool call ID",
		},
		{
			name:      "blank call ID",
			choice:    `{"message":{"role":"assistant","tool_calls":[{"id":"  ","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}`,
			wantError: "tool call ID is required",
		},
		{
			name:      "unsupported call type",
			choice:    `{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"custom","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}`,
			wantError: "unsupported tool call type",
		},
		{
			name:      "blank function name",
			choice:    `{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"  ","arguments":"{}"}}]},"finish_reason":"tool_calls"}`,
			wantError: "has no function name",
		},
		{
			name:   "valid length response",
			choice: `{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}`,
		},
		{
			name:   "missing finish reason with content",
			choice: `{"message":{"role":"assistant","content":"answer"}}`,
		},
		{
			name:      "missing finish reason with tool calls",
			choice:    `{"message":{"role":"assistant","tool_calls":[` + validCall + `]}}`,
			wantCalls: 1,
		},
		{
			name:      "valid tool calls",
			choice:    `{"message":{"role":"assistant","tool_calls":[` + validCall + `]},"finish_reason":"tool_calls"}`,
			wantCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[` + test.choice + `]}`))
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL)
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Complete() error = %v, want error containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			if len(message.ToolCalls) != test.wantCalls {
				t.Errorf("tool calls = %d (%#v), want %d", len(message.ToolCalls), message, test.wantCalls)
			}
		})
	}
}

func TestConversationRejectsDuplicateToolCallIDsBeforeExecution(t *testing.T) {
	executions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","tool_calls":[
				{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}},
				{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}
			]},"finish_reason":"tool_calls"}]
		}`))
	}))
	defer server.Close()

	conversation := newCountingToolConversation(t, server.URL, &executions)
	err := conversation.Send(context.Background(), "question")
	if err == nil || !strings.Contains(err.Error(), "duplicate tool call ID") {
		t.Fatalf("Send() error = %v, want duplicate ID error", err)
	}
	if executions != 0 {
		t.Errorf("tool execution count = %d, want 0", executions)
	}
	if messages := conversation.Messages(); len(messages) != 2 || messages[1].Role != "user" {
		t.Errorf("conversation messages = %#v, want only system and user", messages)
	}
}

func TestConversationChecksToolRoundLimitBeforeExecution(t *testing.T) {
	executions := 0
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprintf(w, `{
			"choices":[{"message":{"role":"assistant","tool_calls":[
				{"id":"call-%d","type":"function","function":{"name":"lookup","arguments":"{}"}}
			]},"finish_reason":"tool_calls"}]
		}`, requests)
	}))
	defer server.Close()

	conversation := newCountingToolConversation(t, server.URL, &executions)
	err := conversation.Send(context.Background(), "question")
	const wantError = "llm: reached maximum of 16 consecutive tool-call rounds"
	if err == nil || err.Error() != wantError {
		t.Fatalf("Send() error = %v, want %q", err, wantError)
	}
	if requests != 17 {
		t.Errorf("model request count = %d, want 17", requests)
	}
	if executions != 16 {
		t.Errorf("tool execution count = %d, want 16", executions)
	}
	if messages := conversation.Messages(); len(messages) != 34 {
		t.Errorf("conversation message count = %d, want 34", len(messages))
	}
}

func TestConversationRespectsConfiguredToolRoundLimit(t *testing.T) {
	executions := 0
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprintf(w, `{
			"choices":[{"message":{"role":"assistant","tool_calls":[
				{"id":"call-%d","type":"function","function":{"name":"lookup","arguments":"{}"}}
			]},"finish_reason":"tool_calls"}]
		}`, requests)
	}))
	defer server.Close()

	conversation := newCountingToolConversation(t, server.URL, &executions)
	conversation.SetMaxToolRounds(3)
	err := conversation.Send(context.Background(), "question")
	const wantError = "llm: reached maximum of 3 consecutive tool-call rounds"
	if err == nil || err.Error() != wantError {
		t.Fatalf("Send() error = %v, want %q", err, wantError)
	}
	if requests != 4 {
		t.Errorf("model request count = %d, want 4", requests)
	}
	if executions != 3 {
		t.Errorf("tool execution count = %d, want 3", executions)
	}
	var limitError *MaxToolRoundsError
	if !errors.As(err, &limitError) || limitError.Limit != 3 {
		t.Errorf("error = %#v, want MaxToolRoundsError with limit 3", err)
	}
}

func TestConversationIgnoresNonPositiveToolRoundLimit(t *testing.T) {
	conversation := &Conversation{maxToolRounds: DefaultMaxToolCallRounds}
	conversation.SetMaxToolRounds(0)
	conversation.SetMaxToolRounds(-5)
	if conversation.maxToolRounds != DefaultMaxToolCallRounds {
		t.Errorf("max tool rounds = %d, want %d", conversation.maxToolRounds, DefaultMaxToolCallRounds)
	}
}

func TestCompleteUsesRequestedModelWhenResponseOmitsModel(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		response string
	}{
		{
			name:     "Chat Completions",
			path:     "/v1/chat/completions",
			response: `{"id":"completion-1","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`,
		},
		{
			name:     "Responses",
			path:     "/v1/responses",
			response: `{"id":"response-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()

			client, err := New("key", "requested-model", server.URL+test.path)
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
			if err != nil {
				t.Fatalf("complete() error: %v", err)
			}
			if message.Model != "requested-model" {
				t.Errorf("message model = %q, want requested-model", message.Model)
			}
		})
	}
}

func TestCompleteFallsBackOnHTTP500AndRemembersWinner(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unsupported protocol", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{
			"model":"configured-model",
			"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	var selected []EndpointType
	client, err := New("key", "configured-model", server.URL+"/v1/responses?api-version=1", ClientOptions{
		EndpointSelected: func(endpointType EndpointType) {
			selected = append(selected, endpointType)
		},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	for range 2 {
		message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
		if err != nil {
			t.Fatalf("complete() error: %v", err)
		}
		if message.Content != "answer" {
			t.Errorf("completion content = %q, want answer", message.Content)
		}
	}

	wantPaths := []string{
		"/v1/responses?api-version=1",
		"/v1/chat/messages?api-version=1",
		"/v1/chat/completions?api-version=1",
		"/v1/chat/completions?api-version=1",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("request paths = %v, want %v", paths, wantPaths)
	}
	if !slices.Equal(selected, []EndpointType{EndpointChatCompletions}) {
		t.Errorf("selected endpoint callbacks = %v, want chat_completions once", selected)
	}
}

func TestCompleteUsesPreferredEndpointFirst(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{
			"role":"assistant",
			"model":"configured-model",
			"content":[{"type":"text","text":"answer"}],
			"stop_reason":"end_turn"
		}`))
	}))
	defer server.Close()

	callbackCalled := false
	client, err := New("key", "configured-model", server.URL+"/v1/responses", ClientOptions{
		PreferredEndpoint: EndpointMessages,
		EndpointSelected: func(EndpointType) {
			callbackCalled = true
		},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
	if err != nil {
		t.Fatalf("complete() error: %v", err)
	}
	if message.Content != "answer" {
		t.Errorf("completion content = %q, want answer", message.Content)
	}
	if !slices.Equal(paths, []string{"/v1/chat/messages"}) {
		t.Errorf("request paths = %v, want Messages only", paths)
	}
	if callbackCalled {
		t.Error("EndpointSelected called for unchanged preferred endpoint")
	}
}

func TestCompleteDoesNotFallbackOnOtherStatus(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/v1/responses")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadGateway {
		t.Fatalf("complete() error = %v, want HTTP 502 APIError", err)
	}
	if !slices.Equal(paths, []string{"/v1/responses"}) {
		t.Errorf("request paths = %v, want no fallback", paths)
	}
}

func TestMessagesConversationWithToolCall(t *testing.T) {
	promptContext := PromptContext{CurrentTime: time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)}
	wantSystemPrompt := systemPromptWithContext(promptContext)
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		if r.URL.RequestURI() != "/v1/chat/messages?api-version=1" {
			t.Errorf("request URI = %q, want Messages endpoint", r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" || r.Header.Get("X-Api-Key") != "secret-key" {
			t.Errorf("authentication headers = Authorization %q, X-Api-Key %q", r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
		}
		if r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("Anthropic-Version = %q", r.Header.Get("Anthropic-Version"))
		}

		var request struct {
			Model     string            `json:"model"`
			MaxTokens int               `json:"max_tokens"`
			System    string            `json:"system"`
			Messages  []messagesMessage `json:"messages"`
			Tools     []messagesTool    `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Model != "configured-model" || request.MaxTokens != messagesMaxTokens {
			t.Errorf("request model/max tokens = %q/%d", request.Model, request.MaxTokens)
		}
		if request.System != wantSystemPrompt {
			t.Errorf("system prompt = %q, want %q", request.System, wantSystemPrompt)
		}
		if len(request.Tools) != 1 || request.Tools[0].Name != "lookup" {
			t.Errorf("tools = %#v, want lookup", request.Tools)
		}

		switch requestNumber {
		case 1:
			if len(request.Messages) != 1 || request.Messages[0].Role != "user" || len(request.Messages[0].Content) != 1 || request.Messages[0].Content[0].Text != "question" {
				t.Errorf("first Messages input = %#v", request.Messages)
			}
			_, _ = w.Write([]byte(`{
				"role":"assistant",
				"model":"configured-model",
				"content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{"key":"value"}}],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":2,"output_tokens":3}
			}`))
		case 2:
			if len(request.Messages) != 3 {
				t.Fatalf("second Messages input length = %d, want 3", len(request.Messages))
			}
			call := request.Messages[1].Content[0]
			if request.Messages[1].Role != "assistant" || call.Type != "tool_use" || call.ID != "call-1" || call.Name != "lookup" || string(call.Input) != `{"key":"value"}` {
				t.Errorf("assistant tool use = %#v", request.Messages[1])
			}
			result := request.Messages[2].Content[0]
			if request.Messages[2].Role != "user" || result.Type != "tool_result" || result.ToolUseID != "call-1" || result.Content != "found value" {
				t.Errorf("user tool result = %#v", request.Messages[2])
			}
			_, _ = w.Write([]byte(`{
				"role":"assistant",
				"model":"configured-model",
				"content":[{"type":"text","text":"answer"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":7,"output_tokens":11}
			}`))
		default:
			t.Fatalf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()

	client, err := New("secret-key", "configured-model", server.URL+"/v1/chat/messages?api-version=1")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	registry, err := tools.NewRegistry(responseTestTool{})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	conversation, err := NewConversation(client, registry, promptContext)
	if err != nil {
		t.Fatalf("NewConversation() error: %v", err)
	}
	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	final := conversation.Messages()[4]
	if final.Content != "answer" || final.TotalTokens == nil || *final.TotalTokens != 18 {
		t.Errorf("final message = %#v, want answer with 18 tokens", final)
	}
	if requestNumber != 2 {
		t.Errorf("request count = %d, want 2", requestNumber)
	}
}

func TestResponsesConversationWithToolCall(t *testing.T) {
	promptContext := PromptContext{
		CurrentTime:    time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC),
		ClientLocation: time.FixedZone("America/New_York", -4*60*60),
	}
	wantSystemPrompt := systemPromptWithContext(promptContext)
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		if r.URL.RequestURI() != "/v1/responses?api-version=1" {
			t.Errorf("request URI = %q, want Responses endpoint", r.URL.RequestURI())
		}

		var request struct {
			Model string `json:"model"`
			Input []struct {
				Type      string `json:"type"`
				Role      string `json:"role"`
				Content   string `json:"content"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Output    string `json:"output"`
			} `json:"input"`
			Tools []struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Model != "configured-model" {
			t.Errorf("model = %q, want configured-model", request.Model)
		}
		if len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Name != "lookup" {
			t.Errorf("tools = %#v, want flattened Responses function tool", request.Tools)
		}

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			if len(request.Input) != 2 || request.Input[0].Role != "system" || request.Input[0].Content != wantSystemPrompt || request.Input[1].Role != "user" || request.Input[1].Content != "question" {
				t.Errorf("first input = %#v, want system prompt and user question", request.Input)
			}
			_, _ = w.Write([]byte(`{
				"id":"response-1",
				"model":"configured-model",
				"status":"completed",
			"output":[
				{"type":"reasoning","id":"reasoning-1","summary":[]},
				{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"key\":\"value\"}"}
			],
				"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}
			}`))
		case 2:
			if len(request.Input) != 5 {
				t.Fatalf("second input length = %d, want 5", len(request.Input))
			}
			if request.Input[0].Role != "system" || request.Input[0].Content != wantSystemPrompt {
				t.Errorf("system input = %#v", request.Input[0])
			}
			if reasoning := request.Input[2]; reasoning.Type != "reasoning" {
				t.Errorf("reasoning input = %#v", reasoning)
			}
			if call := request.Input[3]; call.Type != "function_call" || call.CallID != "call-1" || call.Name != "lookup" || call.Arguments != `{"key":"value"}` {
				t.Errorf("function call input = %#v", call)
			}
			if output := request.Input[4]; output.Type != "function_call_output" || output.CallID != "call-1" || output.Output != "found value" {
				t.Errorf("function output input = %#v", output)
			}
			_, _ = w.Write([]byte(`{
				"id":"response-2",
				"model":"configured-model",
				"status":"completed",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],
				"usage":{"input_tokens":7,"output_tokens":11,"total_tokens":18}
			}`))
		default:
			t.Fatalf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()

	client, err := New("secret-key", "configured-model", server.URL+"/v1/responses?api-version=1")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	registry, err := tools.NewRegistry(responseTestTool{})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	conversation, err := NewConversation(client, registry, promptContext)
	if err != nil {
		t.Fatalf("NewConversation() error: %v", err)
	}
	var toolCallLog bytes.Buffer
	conversation.SetToolCallLogger(log.New(&toolCallLog, "", 0))
	var toolCallStates []bool
	var toolCallResults []string
	conversation.SetToolCallObserver(func(call ToolCall, running bool, result string) {
		if call.Function.Name != "lookup" {
			t.Errorf("observed tool = %q, want lookup", call.Function.Name)
		}
		toolCallStates = append(toolCallStates, running)
		toolCallResults = append(toolCallResults, result)
	})

	err = conversation.Send(context.Background(), "question")
	if err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	messages := conversation.Messages()
	final := messages[len(messages)-1]
	if final.Role != "assistant" || final.Content != "answer" {
		t.Errorf("final message = %#v, want assistant answer", final)
	}
	if final.TotalTokens == nil || *final.TotalTokens != 18 {
		t.Errorf("total tokens = %v, want 18", final.TotalTokens)
	}
	if requestNumber != 2 {
		t.Errorf("request count = %d, want 2", requestNumber)
	}
	wantLog := "tool call: name=\"lookup\" arguments=\"{\\\"key\\\":\\\"value\\\"}\" response=\"found value\"\n"
	if toolCallLog.String() != wantLog {
		t.Errorf("tool call log = %q, want %q", toolCallLog.String(), wantLog)
	}
	if len(toolCallStates) != 2 || !toolCallStates[0] || toolCallStates[1] {
		t.Errorf("tool call states = %v, want [true false]", toolCallStates)
	}
	if len(toolCallResults) != 2 || toolCallResults[0] != "" || toolCallResults[1] != "found value" {
		t.Errorf("tool call results = %v, want empty running result then found value", toolCallResults)
	}
}

func TestProviderMetadataJSONRoundTrip(t *testing.T) {
	metadata, err := NewResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"reasoning-1","encrypted_content":"opaque"}`),
	})
	if err != nil {
		t.Fatalf("NewResponsesProviderMetadata() error: %v", err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal provider metadata: %v", err)
	}
	var restored ProviderMetadata
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("unmarshal provider metadata: %v", err)
	}
	output := restored.ResponsesOutput()
	if len(output) != 1 || !strings.Contains(string(output[0]), `"encrypted_content":"opaque"`) {
		t.Fatalf("restored output = %s, want opaque reasoning item", output)
	}
	output[0][0] = '['
	if fresh := restored.ResponsesOutput(); len(fresh) != 1 || fresh[0][0] != '{' {
		t.Fatal("ResponsesOutput() did not return a deep copy")
	}

	for _, invalid := range []string{
		`{}`,
		`{"responses_output":null}`,
		`{"responses_output":[]}`,
		`{"responses_output":[null]}`,
		`{"responses_output":[{"id":"missing-type"}]}`,
	} {
		if err := json.Unmarshal([]byte(invalid), &restored); err == nil {
			t.Errorf("UnmarshalJSON(%s) error = nil, want validation error", invalid)
		}
	}
}

func TestModelsFromResponsesEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/models?api-version=1" {
			t.Errorf("request URI = %q, want models endpoint", r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()

	client, err := New("key", "model-a", server.URL+"/v1/responses?api-version=1")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if len(models) != 1 || models[0] != "model-a" {
		t.Errorf("Models() = %#v, want model-a", models)
	}
}

type responseTestTool struct{}

func (responseTestTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "lookup",
		Description: "Looks up a value",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`),
	}
}

func (responseTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "found value", nil
}

type countingConversationTool struct {
	executions *int
}

func (countingConversationTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "lookup",
		Description: "Counts executions",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
}

func (tool countingConversationTool) Execute(context.Context, json.RawMessage) (string, error) {
	(*tool.executions)++
	return "result", nil
}

func newCountingToolConversation(t *testing.T, endpoint string, executions *int) *Conversation {
	t.Helper()

	client, err := New("key", "model", endpoint)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	registry, err := tools.NewRegistry(countingConversationTool{executions: executions})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	conversation, err := NewConversation(client, registry, PromptContext{CurrentTime: time.Now()})
	if err != nil {
		t.Fatalf("NewConversation() error: %v", err)
	}
	return conversation
}

func TestImageMessageCloneCopiesData(t *testing.T) {
	data := []byte{1, 2, 3}
	got := cloneMessage(Message{Images: []UserImage{{Data: data}}})
	got.Images[0].Data[0] = 9
	if data[0] != 1 {
		t.Fatal("clone changed source image data")
	}
}

func TestModelInfosCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"unknown"},{"id":"image","architecture":{"input_modalities":["text","image"]}},{"id":"text","architecture":{"input_modalities":["text"]}},{"id":"explicit-image","capabilities":{"image_input":{"supported":true}},"architecture":{"input_modalities":["text"]}},{"id":"explicit-text","capabilities":{"image_input":{"supported":false}},"architecture":{"input_modalities":["text","image"]}}]}`))
	}))
	defer server.Close()
	client, err := New("key", "model", server.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ModelInfos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelInfo{{ID: "unknown", ImageSupport: ImageSupportUnknown}, {ID: "image", ImageSupport: ImageSupportSupported}, {ID: "text", ImageSupport: ImageSupportUnsupported}, {ID: "explicit-image", ImageSupport: ImageSupportSupported}, {ID: "explicit-text", ImageSupport: ImageSupportUnsupported}}
	if !slices.Equal(got, want) {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
}

func TestAnthropicModelInfosPagination(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization")+" key="+r.Header.Get("X-Api-Key")+" version="+r.Header.Get("Anthropic-Version"))
		if r.URL.Query().Get("after_id") == "one" {
			_, _ = w.Write([]byte(`{"data":[{"id":"two"},{"id":"one"}],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"one","capabilities":{"image_input":{"supported":true}}},{"id":"one","capabilities":{"image_input":{"supported":false}}}],"has_more":true,"last_id":"one"}`))
	}))
	defer server.Close()
	client, err := New("key", "model", server.URL+"/v1/chat/messages?api-version=1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ModelInfos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(requests, []string{"GET /v1/models?api-version=1 auth=Bearer key key=key version=2023-06-01", "GET /v1/models?after_id=one&api-version=1 auth=Bearer key key=key version=2023-06-01"}) {
		t.Fatalf("requests = %#v", requests)
	}
	if !slices.Equal(got, []ModelInfo{{ID: "one", ImageSupport: ImageSupportSupported}, {ID: "two", ImageSupport: ImageSupportUnknown}}) {
		t.Fatalf("models = %#v", got)
	}
}

func TestAnthropicModelInfosMalformedPagination(t *testing.T) {
	for _, response := range []string{`{"data":[],"has_more":true}`, `{"data":[],"has_more":true,"last_id":"same"}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
			defer server.Close()
			client, _ := New("key", "model", server.URL+"/v1/chat/messages")
			_, err := client.ModelInfos(context.Background())
			if err == nil || !strings.Contains(err.Error(), "models response") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCompleteImageSerializers(t *testing.T) {
	for _, test := range []struct{ name, suffix string }{{"responses", "/v1/responses"}, {"messages", "/v1/chat/messages"}, {"chat", "/v1/chat/completions"}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(body)
				wire := string(encoded)
				if !strings.Contains(wire, "AQID") {
					t.Errorf("image encoding missing: %s", wire)
				}
				if test.name != "messages" && !strings.Contains(wire, "data:image/png;base64,AQID") {
					t.Errorf("canonical image URL missing: %s", wire)
				}
				if strings.Contains(wire, `"Images"`) {
					t.Error("raw Images field present")
				}
				if test.name == "responses" && !strings.Contains(wire, `"type":"input_image"`) {
					t.Error("missing Responses image part")
				}
				if test.name == "messages" && !strings.Contains(wire, `"type":"image"`) {
					t.Error("missing Messages image block")
				}
				if test.name == "chat" && !strings.Contains(wire, `"type":"image_url"`) {
					t.Error("missing Chat image part")
				}
				switch test.name {
				case "responses":
					_, _ = w.Write([]byte(`{"model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
				case "messages":
					_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
				default:
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
				}
			}))
			defer server.Close()
			client, err := New("key", "model", server.URL+test.suffix)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.complete(context.Background(), []Message{{Role: "user", Content: "text", Images: []UserImage{{Data: []byte{1, 2, 3}, MediaType: "image/png"}}}}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTextOnlyContentRemainsString(t *testing.T) {
	for _, suffix := range []string{"/v1/responses", "/v1/chat/completions"} {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw := string(mustJSON(body))
			if strings.Contains(raw, `"content":[`) {
				t.Errorf("array content: %s", raw)
			}
			if suffix[4] == 'r' {
				_, _ = w.Write([]byte(`{"model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
			} else {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}
		}))
		client, _ := New("key", "model", server.URL+suffix)
		_, err := client.complete(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
		if err != nil || !called {
			t.Fatalf("complete = %v, called=%v", err, called)
		}
		server.Close()
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestImageValidationMakesNoRequest(t *testing.T) {
	for _, suffix := range []string{"/v1/responses", "/v1/chat/messages", "/v1/chat/completions"} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		client, _ := New("key", "model", server.URL+suffix)
		_, err := client.complete(context.Background(), []Message{{Role: "assistant", Images: []UserImage{{Data: []byte{1}, MediaType: "image/png"}}}}, nil)
		server.Close()
		if err == nil || calls != 0 {
			t.Fatalf("%s error=%v calls=%d", suffix, err, calls)
		}
	}
}

func TestAnthropicImageOnlyHasNoEmptyTextBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(string(mustJSON(body)), `"type":"text"`) {
			t.Error("empty text block present")
		}
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer server.Close()
	client, _ := New("key", "model", server.URL+"/v1/chat/messages")
	if _, err := client.complete(context.Background(), []Message{{Role: "user", Images: []UserImage{{Data: []byte{1}, MediaType: "image/png"}}}}, nil); err != nil {
		t.Fatal(err)
	}
}
