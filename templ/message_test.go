package templates

import (
	"bytes"
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

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

func TestFormatMessageCost(t *testing.T) {
	tests := []struct {
		name      string
		cost      float64
		estimated bool
		want      string
	}{
		{name: "provider", cost: 0.25, want: "$0.2500"},
		{name: "estimated", cost: 0.0025, estimated: true, want: "~$0.0025"},
		{name: "tiny positive", cost: 0.000001, estimated: true, want: "~<$0.0001"},
		{name: "zero", cost: 0, want: "$0.0000"},
		{name: "negative", cost: -0.25, want: "$-0.2500"},
		{name: "large", cost: 123456789.98765, want: "$123456789.9877"},
		{name: "rounding boundary", cost: 0.00005, want: "$0.0001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatMessageCost(test.cost, test.estimated); got != test.want {
				t.Errorf("formatMessageCost(%v, %v) = %q, want %q", test.cost, test.estimated, got, test.want)
			}
		})
	}
}

func TestRollingNumberValueMarkupAndKeys(t *testing.T) {
	tokens := int(^uint(0) >> 1)
	cost := 123456789.98765
	message := llm.Message{ID: 23, Role: "assistant", Model: "model", TotalTokens: &tokens, Cost: &cost}
	var output bytes.Buffer
	if err := Messages("42", message).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	markup := output.String()
	for _, want := range []string{
		`data-rolling-number="` + strconv.Itoa(tokens) + `" data-rolling-digits="0" data-rolling-key="message-42-23-tokens">` + strconv.Itoa(tokens),
		`data-rolling-number="123456789.9877" data-rolling-digits="4" data-rolling-key="message-42-23-cost">123456789.9877`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("markup missing %q: %s", want, markup)
		}
	}

	output.Reset()
	if err := Messages("", llm.Message{Role: "assistant", Model: "model", TotalTokens: &tokens, Cost: &cost}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "data-rolling-key=") {
		t.Errorf("anonymous message has stable rolling keys: %s", output.String())
	}

	tinyCost := 0.000001
	output.Reset()
	if err := Messages("42", llm.Message{ID: 24, Role: "assistant", Model: "model", Cost: &tinyCost, CostEstimated: true}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `~&lt;$<span class="rolling-number" data-rolling-number="0.0001" data-rolling-digits="4" data-rolling-key="message-42-24-cost">0.0001</span>`) {
		t.Errorf("estimated tiny cost prefix or numeric fallback changed: %s", output.String())
	}
	if got := messageMetaText(t, output.String()); !reflect.DeepEqual(got, []string{"~<$0.0001"}) {
		t.Errorf("estimated tiny visible cost = %v, want [~<$0.0001]", got)
	}
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
				gotMeta := messageMetaText(t, output.String())
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

func messageMetaText(t *testing.T, markup string) []string {
	t.Helper()
	document, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "span" {
			for _, attribute := range node.Attr {
				if attribute.Key == "class" && strings.Contains(" "+attribute.Val+" ", " message-meta ") {
					text := strings.Join(strings.Fields(htmlNodeText(node)), " ")
					result = append(result, strings.TrimPrefix(text, "· "))
					return
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return result
}

func htmlNodeText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var text strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(htmlNodeText(child))
	}
	return text.String()
}
