package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newUsageServer(t *testing.T, path, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("request path = %q, want %q", r.URL.Path, path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func TestChatCompletionsDetailedUsage(t *testing.T) {
	server := newUsageServer(t, "/v1/chat/completions", `{
		"model":"test-model",
		"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,
			"prompt_tokens_details":{"cached_tokens":40},
			"completion_tokens_details":{"reasoning_tokens":5}}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intValue(message.InputTokens) != 100 || intValue(message.OutputTokens) != 20 ||
		intValue(message.CachedInputTokens) != 40 || intValue(message.ReasoningTokens) != 5 ||
		intValue(message.TotalTokens) != 120 {
		t.Fatalf("usage pointers = input %d output %d cached %d reasoning %d total %d",
			intValue(message.InputTokens), intValue(message.OutputTokens), intValue(message.CachedInputTokens),
			intValue(message.ReasoningTokens), intValue(message.TotalTokens))
	}
}

func TestChatCompletionsPromptCacheHitFallback(t *testing.T) {
	server := newUsageServer(t, "/v1/chat/completions", `{
		"model":"test-model",
		"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":40}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intValue(message.CachedInputTokens) != 40 {
		t.Fatalf("cached input tokens = %d, want 40", intValue(message.CachedInputTokens))
	}
}

func TestMessagesDetailedUsage(t *testing.T) {
	server := newUsageServer(t, "/v1/chat/messages", `{
		"role":"assistant","model":"test-model",
		"content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":40,"cache_creation_input_tokens":10}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/chat/messages")
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intValue(message.InputTokens) != 150 || intValue(message.OutputTokens) != 20 ||
		intValue(message.CachedInputTokens) != 40 || intValue(message.CacheWriteTokens) != 10 ||
		intValue(message.TotalTokens) != 170 {
		t.Fatalf("usage pointers = input %d output %d cached %d write %d total %d",
			intValue(message.InputTokens), intValue(message.OutputTokens), intValue(message.CachedInputTokens),
			intValue(message.CacheWriteTokens), intValue(message.TotalTokens))
	}
}

func TestResponsesDetailedUsage(t *testing.T) {
	server := newUsageServer(t, "/v1/responses", `{
		"model":"test-model",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}],
		"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,
			"input_tokens_details":{"cached_tokens":40},
			"output_tokens_details":{"reasoning_tokens":5}}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intValue(message.InputTokens) != 100 || intValue(message.OutputTokens) != 20 ||
		intValue(message.CachedInputTokens) != 40 || intValue(message.ReasoningTokens) != 5 ||
		intValue(message.TotalTokens) != 120 {
		t.Fatalf("usage pointers = input %d output %d cached %d reasoning %d total %d",
			intValue(message.InputTokens), intValue(message.OutputTokens), intValue(message.CachedInputTokens),
			intValue(message.ReasoningTokens), intValue(message.TotalTokens))
	}
}

func TestUsageTotalFallsBackToInputPlusOutput(t *testing.T) {
	server := newUsageServer(t, "/v1/chat/completions", `{
		"model":"test-model",
		"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":3}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intValue(message.TotalTokens) != 10 {
		t.Fatalf("total tokens = %d, want 10", intValue(message.TotalTokens))
	}
}

func TestCostEstimatorPrecedence(t *testing.T) {
	tests := []struct {
		name              string
		usage             string
		openCodeGo        bool
		wantCost          float64
		wantEstimated     bool
		wantEstimatorRuns int
	}{
		{
			name:              "provider cost wins",
			usage:             `{"prompt_tokens":10,"completion_tokens":5,"cost":0.25}`,
			wantCost:          0.25,
			wantEstimatorRuns: 0,
		},
		{
			name:              "estimator fills missing provider cost",
			usage:             `{"prompt_tokens":10,"completion_tokens":5}`,
			wantCost:          0.5,
			wantEstimated:     true,
			wantEstimatorRuns: 1,
		},
		{
			name:              "OpenCode list cost wins",
			usage:             `{"prompt_tokens":10,"completion_tokens":5,"cost":0.25,"opencode":{"billing_source":"go","list_cost":0.42}}`,
			wantCost:          0.42,
			wantEstimatorRuns: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newUsageServer(t, "/v1/chat/completions", `{
				"model":"test-model",
				"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
				"usage":`+test.usage+`
			}`)
			runs := 0
			client, err := New("key", "test-model", server.URL+"/v1/chat/completions", ClientOptions{
				EstimateCost: func(Usage) (float64, bool) {
					runs++
					return 0.5, true
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			client.openCodeGo = test.openCodeGo
			message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if message.Cost == nil || *message.Cost != test.wantCost || message.CostEstimated != test.wantEstimated {
				t.Fatalf("cost = %v estimated %v, want %v %v", message.Cost, message.CostEstimated, test.wantCost, test.wantEstimated)
			}
			if runs != test.wantEstimatorRuns {
				t.Fatalf("estimator runs = %d, want %d", runs, test.wantEstimatorRuns)
			}
		})
	}
}

func TestCostEstimatorUnavailableLeavesCostNil(t *testing.T) {
	server := newUsageServer(t, "/v1/chat/completions", `{
		"model":"test-model",
		"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}
	}`)
	client, err := New("key", "test-model", server.URL+"/v1/chat/completions", ClientOptions{
		EstimateCost: func(Usage) (float64, bool) { return 0, false },
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Cost != nil || message.CostEstimated {
		t.Fatalf("cost = %v estimated %v, want nil false", message.Cost, message.CostEstimated)
	}
}
