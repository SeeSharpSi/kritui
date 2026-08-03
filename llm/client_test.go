package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
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
	if transport.ResponseHeaderTimeout != providerResponseHeadTimeout {
		t.Errorf("response header timeout = %s, want %s", transport.ResponseHeaderTimeout, providerResponseHeadTimeout)
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
				_, err := client.Complete(ctx, []Message{{Role: "user", Content: "Hello"}})
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
				_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "Hello"}})
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
			completion, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "Hello"}})
			if err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			if completion.Model != "requested-model" || completion.Message.Model != "requested-model" {
				t.Errorf("completion model = %q, message model = %q; want requested-model", completion.Model, completion.Message.Model)
			}
		})
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
	conversation.SetToolCallObserver(func(call ToolCall, running bool) {
		if call.Function.Name != "lookup" {
			t.Errorf("observed tool = %q, want lookup", call.Function.Name)
		}
		toolCallStates = append(toolCallStates, running)
	})

	completion, err := conversation.Send(context.Background(), "question")
	if err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	if completion.ID != "response-2" || completion.Message.Content != "answer" || completion.FinishReason != "stop" {
		t.Errorf("completion = %#v, want final Responses answer", completion)
	}
	if completion.Usage != (Usage{PromptTokens: 7, CompletionTokens: 11, TotalTokens: 18}) {
		t.Errorf("usage = %#v, want Responses token counts", completion.Usage)
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
}

func TestProviderMetadataJSONRoundTrip(t *testing.T) {
	metadata, err := newResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"reasoning-1","encrypted_content":"opaque"}`),
	})
	if err != nil {
		t.Fatalf("newResponsesProviderMetadata() error: %v", err)
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
