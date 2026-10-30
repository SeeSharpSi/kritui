package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"seesharpsi/kritui/tools"
)

type boundaryTestTool struct {
	executions *int
}

func (boundaryTestTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "boundary_lookup",
		Description: "Looks up a boundary test value",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
}

func (tool boundaryTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	if tool.executions != nil {
		(*tool.executions)++
	}
	return "tool result", nil
}

type boundaryWireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	ToolCallID string     `json:"tool_call_id"`
}

type boundaryWireRequest struct {
	Model    string                `json:"model"`
	Messages []boundaryWireMessage `json:"messages"`
}

func decodeBoundaryRequest(t *testing.T, r *http.Request) boundaryWireRequest {
	t.Helper()

	var request boundaryWireRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatalf("decode boundary request: %v", err)
	}
	return request
}

func boundaryToolCallResponse(callID string, totalTokens int) string {
	return fmt.Sprintf(`{
		"model":"model",
		"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":%q,"type":"function","function":{"name":"boundary_lookup","arguments":"{}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"total_tokens":%d}
	}`, callID, totalTokens)
}

func boundaryFinalResponse(content string, totalTokens int) string {
	return fmt.Sprintf(`{
		"model":"model",
		"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],
		"usage":{"total_tokens":%d}
	}`, content, totalTokens)
}

func newBoundaryTestConversation(t *testing.T, endpoint string, executions *int, promptContext PromptContext) *Conversation {
	t.Helper()

	client, err := New("key", "model", endpoint)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	registry, err := tools.NewRegistry(boundaryTestTool{executions: executions})
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	conversation, err := NewConversation(client, registry, promptContext)
	if err != nil {
		t.Fatalf("NewConversation() error: %v", err)
	}
	return conversation
}

func TestConversationRoundBoundaryInitial(t *testing.T) {
	promptContext := PromptContext{CurrentTime: time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)}
	refreshedContext := PromptContext{CurrentTime: time.Date(2026, time.October, 8, 9, 30, 0, 0, time.UTC)}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request := decodeBoundaryRequest(t, r)
		if requests != 1 {
			t.Fatalf("unexpected request %d", requests)
		}
		if len(request.Messages) != 3 {
			t.Fatalf("request messages = %#v, want three messages", request.Messages)
		}
		if request.Messages[0].Role != "system" || request.Messages[0].Content != systemPromptWithContext(refreshedContext) {
			t.Errorf("system message = %#v, want refreshed prompt context", request.Messages[0])
		}
		if request.Messages[1].Role != "user" || request.Messages[1].Content != "question" {
			t.Errorf("user message = %#v, want question", request.Messages[1])
		}
		if request.Messages[2].Role != "user" || request.Messages[2].Content != "initial steering" {
			t.Errorf("injected message = %#v, want initial steering", request.Messages[2])
		}
		_, _ = w.Write([]byte(boundaryFinalResponse("answer", 4)))
	}))
	defer server.Close()

	conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, promptContext)
	boundaries := 0
	conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
		boundaries++
		switch boundaries {
		case 1:
			if boundary.Generated != nil {
				t.Errorf("initial Generated = %#v, want nil", boundary.Generated)
			}
			if !boundary.NeedsContinuation {
				t.Error("initial NeedsContinuation = false, want true")
			}
			return ConversationInput{
				Messages:      []Message{{Role: "user", Content: "initial steering"}},
				PromptContext: &refreshedContext,
			}, nil
		case 2:
			if len(boundary.Generated) != 1 || boundary.Generated[0].Content != "answer" {
				t.Errorf("final Generated = %#v, want answer", boundary.Generated)
			}
			if boundary.NeedsContinuation {
				t.Error("final NeedsContinuation = true, want false")
			}
			return ConversationInput{}, nil
		default:
			t.Fatalf("unexpected boundary %d", boundaries)
			return ConversationInput{}, nil
		}
	})

	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	if requests != 1 || boundaries != 2 {
		t.Errorf("requests = %d, boundaries = %d, want 1 and 2", requests, boundaries)
	}
	messages := conversation.Messages()
	if len(messages) != 4 {
		t.Fatalf("history length = %d, want 4: %#v", len(messages), messages)
	}
	if messages[0].Content != systemPromptWithContext(refreshedContext) ||
		messages[1].Content != "question" ||
		messages[2].Content != "initial steering" ||
		messages[3].Content != "answer" {
		t.Errorf("history = %#v, want refreshed system, question, steering, answer", messages)
	}
}

func TestConversationRoundBoundaryPreservesCustomSystemPrompt(t *testing.T) {
	custom := "Custom account base stays in place."
	initialContext := PromptContext{
		SystemPrompt: custom,
		CurrentTime:  time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC),
	}
	refreshedContext := PromptContext{
		SystemPrompt: custom,
		CurrentTime:  time.Date(2026, time.October, 8, 9, 30, 0, 0, time.UTC),
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request := decodeBoundaryRequest(t, r)
		if request.Messages[0].Role != "system" ||
			request.Messages[0].Content != systemPromptWithContext(refreshedContext) {
			t.Errorf("system message = %#v, want refreshed custom prompt", request.Messages[0])
		}
		if !strings.Contains(request.Messages[0].Content, custom) {
			t.Errorf("system message dropped custom base: %q", request.Messages[0].Content)
		}
		if !strings.Contains(request.Messages[0].Content, invariantPDFInstructions) {
			t.Errorf("system message dropped invariant PDF guidance: %q", request.Messages[0].Content)
		}
		_, _ = w.Write([]byte(boundaryFinalResponse("answer", 4)))
	}))
	defer server.Close()

	conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, initialContext)
	boundaries := 0
	conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
		boundaries++
		switch boundaries {
		case 1:
			return ConversationInput{
				Messages:      []Message{{Role: "user", Content: "initial steering"}},
				PromptContext: &refreshedContext,
			}, nil
		case 2:
			return ConversationInput{}, nil
		default:
			t.Fatalf("unexpected boundary %d", boundaries)
			return ConversationInput{}, nil
		}
	})

	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
	messages := conversation.Messages()
	if len(messages) == 0 || messages[0].Content != systemPromptWithContext(refreshedContext) {
		t.Errorf("history system message = %#v, want refreshed custom prompt", messages)
	}
}

func TestConversationRoundBoundarySteersAfterToolBatch(t *testing.T) {
	requests := 0
	executions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request := decodeBoundaryRequest(t, r)
		switch requests {
		case 1:
			if len(request.Messages) != 2 || request.Messages[1].Content != "question" {
				t.Fatalf("first request messages = %#v", request.Messages)
			}
			_, _ = w.Write([]byte(boundaryToolCallResponse("call-1", 5)))
		case 2:
			if len(request.Messages) != 5 {
				t.Fatalf("second request message count = %d, want 5: %#v", len(request.Messages), request.Messages)
			}
			assistant := request.Messages[2]
			if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" {
				t.Errorf("assistant tool call = %#v", assistant)
			}
			toolResult := request.Messages[3]
			if toolResult.Role != "tool" || toolResult.ToolCallID != "call-1" || toolResult.Content != "tool result" {
				t.Errorf("tool result = %#v", toolResult)
			}
			steering := request.Messages[4]
			if steering.Role != "user" || steering.Content != "steer now" {
				t.Errorf("steering message = %#v", steering)
			}
			_, _ = w.Write([]byte(boundaryFinalResponse("final answer", 9)))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", &executions, PromptContext{CurrentTime: time.Now()})
	steering := Message{Role: "user", Content: "steer now"}
	boundaries := 0
	conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
		boundaries++
		switch boundaries {
		case 1:
			if boundary.Generated != nil || !boundary.NeedsContinuation {
				t.Errorf("initial boundary = %#v", boundary)
			}
			return ConversationInput{}, nil
		case 2:
			if !boundary.NeedsContinuation {
				t.Error("tool batch NeedsContinuation = false, want true")
			}
			if len(boundary.Generated) != 2 {
				t.Fatalf("tool batch Generated = %#v, want assistant and tool result", boundary.Generated)
			}
			assistant := boundary.Generated[0]
			if assistant.Role != "assistant" || assistant.Model != "model" || assistant.TotalTokens == nil || *assistant.TotalTokens != 5 {
				t.Errorf("generated assistant metadata = %#v, want model and 5 tokens", assistant)
			}
			if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" {
				t.Errorf("generated assistant tool calls = %#v", assistant.ToolCalls)
			}
			toolResult := boundary.Generated[1]
			if toolResult.Role != "tool" || toolResult.ToolCallID != "call-1" || toolResult.Content != "tool result" {
				t.Errorf("generated tool result = %#v", toolResult)
			}
			boundary.Generated[0].Content = "mutated assistant"
			boundary.Generated[1].Content = "mutated tool result"
			return ConversationInput{Messages: []Message{steering}}, nil
		case 3:
			if boundary.NeedsContinuation {
				t.Error("final NeedsContinuation = true, want false")
			}
			if len(boundary.Generated) != 1 || boundary.Generated[0].Content != "final answer" {
				t.Errorf("final Generated = %#v, want final answer only", boundary.Generated)
			}
			return ConversationInput{}, nil
		default:
			t.Fatalf("unexpected boundary %d", boundaries)
			return ConversationInput{}, nil
		}
	})

	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	steering.Content = "mutated steering"
	if requests != 2 || boundaries != 3 || executions != 1 {
		t.Errorf("requests = %d, boundaries = %d, executions = %d, want 2, 3, and 1", requests, boundaries, executions)
	}
	messages := conversation.Messages()
	if len(messages) != 6 {
		t.Fatalf("history length = %d, want 6: %#v", len(messages), messages)
	}
	if messages[2].Content != "" || messages[3].Content != "tool result" {
		t.Errorf("generated history changed through boundary clone: %#v", messages[2:4])
	}
	if messages[2].ToolCalls[0].ID != "call-1" || messages[3].ToolCallID != "call-1" ||
		messages[4].Content != "steer now" || messages[5].Content != "final answer" {
		t.Errorf("history = %#v, want tool call, tool result, steering, final answer", messages)
	}
	if messages[2].TotalTokens == nil || *messages[2].TotalTokens != 5 {
		t.Errorf("persisted assistant tokens = %v, want 5", messages[2].TotalTokens)
	}
}

func TestConversationRoundBoundarySteersAfterFinalResponseInOrder(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request := decodeBoundaryRequest(t, r)
		switch requests {
		case 1:
			_, _ = w.Write([]byte(boundaryFinalResponse("first answer", 3)))
		case 2:
			if len(request.Messages) != 6 {
				t.Fatalf("second request message count = %d, want 6: %#v", len(request.Messages), request.Messages)
			}
			if request.Messages[2].Content != "first answer" {
				t.Errorf("first assistant = %#v", request.Messages[2])
			}
			if request.Messages[3].Content != "follow up one" || request.Messages[4].Content != "follow up two" || request.Messages[5].Content != "follow up three" {
				t.Errorf("injected messages = %#v, want three follow-ups in order", request.Messages[3:])
			}
			_, _ = w.Write([]byte(boundaryFinalResponse("second answer", 6)))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, PromptContext{CurrentTime: time.Now()})
	boundaries := 0
	conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
		boundaries++
		switch boundaries {
		case 1:
			return ConversationInput{}, nil
		case 2:
			if boundary.NeedsContinuation {
				t.Error("first final NeedsContinuation = true, want false")
			}
			if len(boundary.Generated) != 1 || boundary.Generated[0].Content != "first answer" {
				t.Errorf("first final Generated = %#v", boundary.Generated)
			}
			if boundary.Generated[0].TotalTokens == nil || *boundary.Generated[0].TotalTokens != 3 {
				t.Errorf("first final tokens = %v, want 3", boundary.Generated[0].TotalTokens)
			}
			return ConversationInput{Messages: []Message{
				{Role: "user", Content: "follow up one"},
				{Role: "user", Content: "follow up two"},
				{Role: "user", Content: "follow up three"},
			}}, nil
		case 3:
			if boundary.NeedsContinuation {
				t.Error("second final NeedsContinuation = true, want false")
			}
			if len(boundary.Generated) != 1 || boundary.Generated[0].Content != "second answer" {
				t.Errorf("second final Generated = %#v, want second answer only", boundary.Generated)
			}
			return ConversationInput{}, nil
		default:
			t.Fatalf("unexpected boundary %d", boundaries)
			return ConversationInput{}, nil
		}
	})

	if err := conversation.Send(context.Background(), "question"); err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	if requests != 2 || boundaries != 3 {
		t.Errorf("requests = %d, boundaries = %d, want 2 and 3", requests, boundaries)
	}
	messages := conversation.Messages()
	if len(messages) != 7 {
		t.Fatalf("history length = %d, want 7: %#v", len(messages), messages)
	}
	for index, want := range []string{"question", "first answer", "follow up one", "follow up two", "follow up three", "second answer"} {
		if messages[index+1].Content != want {
			t.Errorf("history[%d] = %q, want %q", index+1, messages[index+1].Content, want)
		}
	}
}

func TestConversationRoundBoundaryErrorStopsProviderCalls(t *testing.T) {
	boundaryError := errors.New("boundary failed")

	t.Run("initial", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_, _ = w.Write([]byte(boundaryFinalResponse("answer", 1)))
		}))
		defer server.Close()

		conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, PromptContext{CurrentTime: time.Now()})
		conversation.SetRoundBoundary(func(context.Context, ConversationBoundary) (ConversationInput, error) {
			return ConversationInput{}, boundaryError
		})

		err := conversation.Send(context.Background(), "question")
		if !errors.Is(err, boundaryError) {
			t.Fatalf("Send() error = %v, want boundary error", err)
		}
		if requests != 0 {
			t.Errorf("requests = %d, want 0", requests)
		}
		if messages := conversation.Messages(); len(messages) != 2 {
			t.Errorf("history = %#v, want system and user only", messages)
		}
	})

	t.Run("after tool batch", func(t *testing.T) {
		requests := 0
		executions := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_, _ = w.Write([]byte(boundaryToolCallResponse("call-1", 2)))
		}))
		defer server.Close()

		conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", &executions, PromptContext{CurrentTime: time.Now()})
		boundaries := 0
		conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
			boundaries++
			if boundaries == 1 {
				if boundary.Generated != nil || !boundary.NeedsContinuation {
					t.Errorf("initial boundary = %#v", boundary)
				}
				return ConversationInput{}, nil
			}
			if len(boundary.Generated) != 2 {
				t.Errorf("tool batch Generated = %#v, want assistant and tool result", boundary.Generated)
			}
			return ConversationInput{}, boundaryError
		})

		err := conversation.Send(context.Background(), "question")
		if !errors.Is(err, boundaryError) {
			t.Fatalf("Send() error = %v, want boundary error", err)
		}
		if requests != 1 || executions != 1 {
			t.Errorf("requests = %d, executions = %d, want 1 and 1", requests, executions)
		}
		if messages := conversation.Messages(); len(messages) != 4 || messages[2].Role != "assistant" || messages[3].Role != "tool" {
			t.Errorf("history = %#v, want generated assistant and tool result", messages)
		}
	})

	t.Run("after final response", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_, _ = w.Write([]byte(boundaryFinalResponse("answer", 1)))
		}))
		defer server.Close()

		conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, PromptContext{CurrentTime: time.Now()})
		boundaries := 0
		conversation.SetRoundBoundary(func(context.Context, ConversationBoundary) (ConversationInput, error) {
			boundaries++
			if boundaries == 1 {
				return ConversationInput{}, nil
			}
			return ConversationInput{Messages: []Message{{Role: "user", Content: "must not be sent"}}}, boundaryError
		})

		err := conversation.Send(context.Background(), "question")
		if !errors.Is(err, boundaryError) {
			t.Fatalf("Send() error = %v, want boundary error", err)
		}
		if requests != 1 {
			t.Errorf("requests = %d, want 1", requests)
		}
		messages := conversation.Messages()
		if len(messages) != 3 || messages[2].Content != "answer" {
			t.Errorf("history = %#v, want system, user, and final assistant", messages)
		}
	})
}

func TestConversationRoundBoundaryResetsToolRoundLimit(t *testing.T) {
	requests := 0
	executions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(boundaryToolCallResponse(fmt.Sprintf("call-%d", requests), requests)))
	}))
	defer server.Close()

	conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", &executions, PromptContext{CurrentTime: time.Now()})
	conversation.SetMaxToolRounds(1)
	boundaries := 0
	conversation.SetRoundBoundary(func(_ context.Context, boundary ConversationBoundary) (ConversationInput, error) {
		boundaries++
		switch boundaries {
		case 1:
			if boundary.Generated != nil || !boundary.NeedsContinuation {
				t.Errorf("initial boundary = %#v", boundary)
			}
			return ConversationInput{}, nil
		case 2:
			if len(boundary.Generated) != 2 || !boundary.NeedsContinuation {
				t.Errorf("first tool boundary = %#v", boundary)
			}
			return ConversationInput{Messages: []Message{{Role: "user", Content: "continue"}}}, nil
		case 3:
			if len(boundary.Generated) != 2 || !boundary.NeedsContinuation {
				t.Errorf("second tool boundary = %#v", boundary)
			}
			return ConversationInput{}, nil
		default:
			t.Fatalf("unexpected boundary %d", boundaries)
			return ConversationInput{}, nil
		}
	})

	err := conversation.Send(context.Background(), "question")
	var limitError *MaxToolRoundsError
	if !errors.As(err, &limitError) || limitError.Limit != 1 {
		t.Fatalf("Send() error = %#v, want MaxToolRoundsError with limit 1", err)
	}
	if requests != 3 || executions != 2 || boundaries != 3 {
		t.Errorf("requests = %d, executions = %d, boundaries = %d, want 3, 2, and 3", requests, executions, boundaries)
	}
	messages := conversation.Messages()
	if len(messages) != 7 {
		t.Fatalf("history length = %d, want 7: %#v", len(messages), messages)
	}
	if messages[4].Role != "user" || messages[4].Content != "continue" {
		t.Errorf("injected message = %#v, want continue", messages[4])
	}
}

func TestConversationRoundBoundaryRejectsInvalidMessages(t *testing.T) {
	tests := []struct {
		name    string
		message Message
		want    string
	}{
		{name: "role", message: Message{Role: "assistant", Content: "not user"}, want: "role must be user"},
		{name: "empty", message: Message{Role: "user"}, want: "must contain content or attachments"},
		{
			name: "attachment",
			message: Message{Role: "user", Attachments: []UserAttachment{{
				Filename:  "notes.txt",
				MediaType: "text/plain",
				Data:      []byte("text"),
			}}},
			want: "unsupported attachment media type",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte(boundaryFinalResponse("answer", 1)))
			}))
			defer server.Close()

			conversation := newBoundaryTestConversation(t, server.URL+"/v1/chat/completions", nil, PromptContext{CurrentTime: time.Now()})
			conversation.SetRoundBoundary(func(context.Context, ConversationBoundary) (ConversationInput, error) {
				return ConversationInput{Messages: []Message{test.message}}, nil
			})

			err := conversation.Send(context.Background(), "question")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Send() error = %v, want error containing %q", err, test.want)
			}
			if requests != 0 {
				t.Errorf("requests = %d, want 0", requests)
			}
			if messages := conversation.Messages(); len(messages) != 2 {
				t.Errorf("history = %#v, want system and user only", messages)
			}
		})
	}
}
