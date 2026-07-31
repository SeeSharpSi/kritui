package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"seesharpsi/kritui/tools"
)

const maxToolCallRounds = 16

// Conversation retains message history and executes tool calls requested by
// the model. A Conversation must not be used concurrently.
type Conversation struct {
	client   *Client
	registry *tools.Registry
	messages []Message
}

// NewConversation creates a conversation with optional existing history. A
// nil registry disables tools.
func NewConversation(client *Client, registry *tools.Registry, messages ...Message) (*Conversation, error) {
	if client == nil {
		return nil, errors.New("llm: conversation client is required")
	}

	return &Conversation{
		client:   client,
		registry: registry,
		messages: cloneMessages(messages),
	}, nil
}

// Messages returns a copy of the conversation history.
func (c *Conversation) Messages() []Message {
	if c == nil {
		return nil
	}
	return cloneMessages(c.messages)
}

// Send adds a user message and completes the conversation, including any tool
// calls needed to produce the final assistant response.
func (c *Conversation) Send(ctx context.Context, content string) (Completion, error) {
	if c == nil || c.client == nil {
		return Completion{}, errors.New("llm: conversation client is required")
	}

	c.messages = append(c.messages, Message{Role: "user", Content: content})
	return c.Complete(ctx)
}

// Complete continues from current history until the model returns a response
// without tool calls. It is useful for initial histories and for retrying a
// failed completion without adding another user message.
func (c *Conversation) Complete(ctx context.Context) (Completion, error) {
	if c == nil || c.client == nil {
		return Completion{}, errors.New("llm: conversation client is required")
	}

	var definitions []tools.Definition
	if c.registry != nil {
		definitions = c.registry.Definitions()
	}

	toolRounds := 0
	for {
		completion, err := c.client.complete(ctx, c.messages, definitions)
		if err != nil {
			return Completion{}, err
		}

		calls := completion.Message.ToolCalls
		if len(calls) == 0 {
			if completion.FinishReason == "tool_calls" {
				return Completion{}, errors.New("llm: response finished with tool_calls but contained no tool calls")
			}
			c.messages = append(c.messages, cloneMessage(completion.Message))
			return completion, nil
		}
		for _, call := range calls {
			if err := validateToolCall(call); err != nil {
				return Completion{}, err
			}
		}
		c.messages = append(c.messages, cloneMessage(completion.Message))

		for _, call := range calls {
			result, err := c.executeToolCall(ctx, call)
			if err != nil {
				return Completion{}, err
			}
			c.messages = append(c.messages, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: call.ID,
			})
		}

		toolRounds++
		if toolRounds >= maxToolCallRounds {
			return Completion{}, fmt.Errorf("llm: exceeded %d consecutive tool-call rounds", maxToolCallRounds)
		}
	}
}

func (c *Conversation) executeToolCall(ctx context.Context, call ToolCall) (string, error) {
	if c.registry == nil {
		return fmt.Sprintf("Tool error: no tools are available; cannot execute %q", call.Function.Name), nil
	}

	result, err := c.registry.Execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return "Tool error: " + err.Error(), nil
}

func validateToolCall(call ToolCall) error {
	if call.ID == "" {
		return errors.New("llm: tool call ID is required")
	}
	if call.Type != "function" {
		return fmt.Errorf("llm: unsupported tool call type %q", call.Type)
	}
	if call.Function.Name == "" {
		return fmt.Errorf("llm: tool call %q has no function name", call.ID)
	}
	return nil
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for index, message := range messages {
		cloned[index] = cloneMessage(message)
	}
	return cloned
}

func cloneMessage(message Message) Message {
	message.ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
	message.responseItems = cloneRawMessages(message.responseItems)
	return message
}
