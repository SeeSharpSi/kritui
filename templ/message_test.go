package templates

import (
	"bytes"
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func usagePointer[T int | float64](value T) *T {
	return &value
}

func TestToolCallElisionPresentation(t *testing.T) {
	const elisionTitle = `title="Tool result elided from model context"`
	render := func(t *testing.T, calls []llm.ToolCall, errors map[string]string) string {
		t.Helper()
		var output bytes.Buffer
		if err := ToolCalls(calls, "", errors).Render(context.Background(), &output); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}

	t.Run("elided success keeps arguments and mutes the name class", func(t *testing.T) {
		call := llm.ToolCall{
			ID: "call-ok", Type: "function", ResultElided: true,
			Function: llm.FunctionCall{Name: "lookup", Arguments: `{"key":"value"}`},
		}
		markup := render(t, []llm.ToolCall{call}, nil)
		for _, want := range []string{`class="tool-call tool-elided"`, elisionTitle, `class="tool-call-toggle"`, "</button>"} {
			if !strings.Contains(markup, want) {
				t.Fatalf("elided success markup missing %q: %s", want, markup)
			}
		}
		if strings.Count(markup, "tool-elided") != 1 {
			t.Fatalf("elision class leaked outside the name: %s", markup)
		}
		if strings.Count(markup, "<code") != 1 || strings.Contains(markup, `<code class=`) {
			t.Fatalf("argument body should remain unclassified: %s", markup)
		}
	})

	t.Run("elided error keeps the original error body", func(t *testing.T) {
		call := llm.ToolCall{
			ID: "call-error", Type: "function", ResultElided: true,
			Function: llm.FunctionCall{Name: "lookup", Arguments: `{"key":"value"}`},
		}
		markup := render(t, []llm.ToolCall{call}, map[string]string{"call-error": "provider rejected input"})
		for _, want := range []string{`class="tool-call tool-error tool-elided"`, elisionTitle, `class="tool-call-error" role="alert"`, "provider rejected input"} {
			if !strings.Contains(markup, want) {
				t.Fatalf("elided error markup missing %q: %s", want, markup)
			}
		}
		if strings.Count(markup, "tool-elided") != 1 {
			t.Fatalf("elision class leaked into the error body: %s", markup)
		}
	})

	t.Run("elided no-argument call marks the strong name", func(t *testing.T) {
		call := llm.ToolCall{
			ID: "call-bare", Type: "function", ResultElided: true,
			Function: llm.FunctionCall{Name: "lookup", Arguments: "{}"},
		}
		markup := render(t, []llm.ToolCall{call}, nil)
		for _, want := range []string{`class="tool-call tool-elided"`, `<strong ` + elisionTitle + `>lookup</strong>`} {
			if !strings.Contains(markup, want) {
				t.Fatalf("elided no-argument markup missing %q: %s", want, markup)
			}
		}
	})

	t.Run("full results keep the normal name presentation", func(t *testing.T) {
		calls := []llm.ToolCall{
			{ID: "call-full", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{"key":"value"}`}},
			{ID: "call-bare", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: "{}"}},
		}
		markup := render(t, calls, nil)
		if strings.Contains(markup, "tool-elided") || strings.Contains(markup, elisionTitle) {
			t.Fatalf("full results gained elision presentation: %s", markup)
		}
		if !strings.Contains(markup, `<strong title="">lookup</strong>`) {
			t.Fatalf("full no-argument name lost its normal presentation: %s", markup)
		}
	})
}

func TestMessagesResponseUsage(t *testing.T) {
	toolRound := func(tokens *int, cost *float64) llm.Message {
		return llm.Message{
			Role: "assistant", TotalTokens: tokens, Cost: cost,
			ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "search", Arguments: `{}`}}},
		}
	}
	answer := func(tokens *int, cost *float64) llm.Message {
		return llm.Message{Role: "assistant", Model: "test-model", Content: "Answer", TotalTokens: tokens, Cost: cost}
	}
	user := llm.Message{Role: "user", Content: "Question"}
	result := llm.Message{Role: "tool", ToolCallID: "call-1", Content: "Result"}
	tests := []struct {
		name     string
		messages []llm.Message
		wantMeta []string
	}{
		{
			name: "multiple tool rounds",
			messages: []llm.Message{
				user, toolRound(usagePointer(5), usagePointer(0.05)), result,
				toolRound(usagePointer(7), usagePointer(0.07)), result,
				answer(usagePointer(18), usagePointer(0.25)),
			},
			wantMeta: []string{"30 tokens · $0.3700"},
		},
		{
			name: "separate answers reset totals",
			messages: []llm.Message{
				user, toolRound(usagePointer(5), usagePointer(0.05)), result,
				answer(usagePointer(18), usagePointer(0.25)),
				user, toolRound(usagePointer(2), usagePointer(0.02)), result,
				answer(usagePointer(3), usagePointer(0.03)),
			},
			wantMeta: []string{"23 tokens · $0.3000", "5 tokens · $0.0500"},
		},
		{
			name: "steering retains preceding round usage",
			messages: []llm.Message{
				user, toolRound(usagePointer(5), usagePointer(0.05)), result,
				user, toolRound(usagePointer(7), usagePointer(0.07)), result,
				answer(usagePointer(11), usagePointer(0.11)),
			},
			wantMeta: []string{"23 tokens · $0.2300"},
		},
		{
			name: "final round has no usage",
			messages: []llm.Message{
				user, toolRound(usagePointer(5), usagePointer(0.05)), result, answer(nil, nil),
			},
			wantMeta: []string{"5 tokens · $0.0500"},
		},
		{
			name: "token and cost availability are independent",
			messages: []llm.Message{
				user, toolRound(usagePointer(5), nil), result, answer(nil, usagePointer(0.25)),
			},
			wantMeta: []string{"5 tokens · $0.2500"},
		},
		{
			name:     "tokens without cost",
			messages: []llm.Message{user, toolRound(usagePointer(5), nil), result, answer(usagePointer(18), nil)},
			wantMeta: []string{"23 tokens"},
		},
		{
			name:     "cost without tokens",
			messages: []llm.Message{user, toolRound(nil, usagePointer(0.05)), result, answer(nil, usagePointer(0.25))},
			wantMeta: []string{"$0.3000"},
		},
		{
			name:     "explicit zero usage remains visible",
			messages: []llm.Message{user, toolRound(usagePointer(0), usagePointer(0.0)), result, answer(nil, nil)},
			wantMeta: []string{"0 tokens · $0.0000"},
		},
		{
			name: "missing usage does not inherit previous answer",
			messages: []llm.Message{
				user, answer(usagePointer(5), usagePointer(0.05)),
				user, toolRound(nil, nil), result, answer(nil, nil),
			},
			wantMeta: []string{"5 tokens · $0.0500"},
		},
		{
			name:     "all usage absent",
			messages: []llm.Message{user, toolRound(nil, nil), result, answer(nil, nil)},
		},
		{
			name: "non-assistant usage is ignored",
			messages: []llm.Message{
				{Role: "system", TotalTokens: usagePointer(100), Cost: usagePointer(1.0)},
				{Role: "user", Content: "Question", TotalTokens: usagePointer(100), Cost: usagePointer(1.0)},
				toolRound(usagePointer(5), usagePointer(0.05)),
				{Role: "tool", ToolCallID: "call-1", Content: "Result", TotalTokens: usagePointer(100), Cost: usagePointer(1.0)},
				answer(usagePointer(18), usagePointer(0.25)),
			},
			wantMeta: []string{"23 tokens · $0.3000"},
		},
	}
	metaPattern := regexp.MustCompile(`<span class="message-meta">· ([^<]+)</span>`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]llm.Message(nil), tt.messages...)
			tokens := make([]int, len(original))
			costs := make([]float64, len(original))
			for i, message := range original {
				if message.TotalTokens != nil {
					tokens[i] = *message.TotalTokens
				}
				if message.Cost != nil {
					costs[i] = *message.Cost
				}
			}
			for render := 0; render < 2; render++ {
				var output bytes.Buffer
				if err := Messages("", tt.messages...).Render(context.Background(), &output); err != nil {
					t.Fatal(err)
				}
				var gotMeta []string
				for _, match := range metaPattern.FindAllStringSubmatch(output.String(), -1) {
					gotMeta = append(gotMeta, match[1])
				}
				if !reflect.DeepEqual(gotMeta, tt.wantMeta) {
					t.Fatalf("render %d metadata = %v, want %v; HTML: %s", render, gotMeta, tt.wantMeta, output.String())
				}
				for i, message := range tt.messages {
					if message.TotalTokens != original[i].TotalTokens || message.Cost != original[i].Cost {
						t.Fatalf("render %d replaced stored usage pointers for message %d", render, i)
					}
					if message.TotalTokens != nil && *message.TotalTokens != tokens[i] {
						t.Fatalf("render %d mutated stored tokens for message %d", render, i)
					}
					if message.Cost != nil && *message.Cost != costs[i] {
						t.Fatalf("render %d mutated stored cost for message %d", render, i)
					}
				}
			}
		})
	}
}
