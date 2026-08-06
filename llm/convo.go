package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"seesharpsi/kritui/tools"
)

const (
	// DefaultMaxToolCallRounds limits consecutive tool-call rounds when no
	// explicit limit is configured.
	DefaultMaxToolCallRounds = 16
	toolErrorPrefix          = "Tool error: "
)

// MaxToolRoundsError reports that consecutive tool-call rounds reached the
// configured limit.
type MaxToolRoundsError struct {
	Limit int
}

func (e *MaxToolRoundsError) Error() string {
	return fmt.Sprintf("llm: reached maximum of %d consecutive tool-call rounds", e.Limit)
}

// Conversation retains message history and executes tool calls requested by
// the model. A Conversation must not be used concurrently.
type Conversation struct {
	client           *Client
	registry         *tools.Registry
	maxToolRounds    int
	toolCallLogger   *log.Logger
	toolCallObserver func(ToolCall, bool, string)
	messages         []Message
}

// SetToolCallLogger configures logging for tool names, arguments, and results.
// Passing nil disables tool-call logging.
func (c *Conversation) SetToolCallLogger(logger *log.Logger) {
	if c != nil {
		c.toolCallLogger = logger
	}
}

// SetToolCallObserver configures a callback invoked when a tool call starts
// and finishes. Result is empty while running and populated when finished.
func (c *Conversation) SetToolCallObserver(observer func(ToolCall, bool, string)) {
	if c != nil {
		c.toolCallObserver = observer
	}
}

// ToolErrorMessage returns error text from a tool result, or an empty string
// when the result does not represent an error.
func ToolErrorMessage(result string) string {
	if !strings.HasPrefix(result, toolErrorPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(result, toolErrorPrefix))
}

// NewConversation creates a conversation with optional existing history. A
// nil registry disables tools.
func NewConversation(client *Client, registry *tools.Registry, promptContext PromptContext, messages ...Message) (*Conversation, error) {
	if client == nil {
		return nil, errors.New("llm: conversation client is required")
	}
	if promptContext.CurrentTime.IsZero() {
		return nil, errors.New("llm: prompt current time is required")
	}

	return &Conversation{
		client:        client,
		registry:      registry,
		maxToolRounds: DefaultMaxToolCallRounds,
		messages: append(
			[]Message{{Role: "system", Content: systemPromptWithContext(promptContext)}},
			cloneMessages(messages)...,
		),
	}, nil
}

// SetMaxToolRounds limits consecutive tool-call rounds executed before a final
// response. Values at or below zero keep the default limit.
func (c *Conversation) SetMaxToolRounds(rounds int) {
	if c != nil && rounds > 0 {
		c.maxToolRounds = rounds
	}
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
		if err := validateAssistantMessage(completion.Message); err != nil {
			return Completion{}, err
		}

		calls := completion.Message.ToolCalls
		if len(calls) == 0 {
			c.messages = append(c.messages, cloneMessage(completion.Message))
			return completion, nil
		}
		if toolRounds >= c.maxToolRounds {
			return Completion{}, &MaxToolRoundsError{Limit: c.maxToolRounds}
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
	}
}

func (c *Conversation) executeToolCall(ctx context.Context, call ToolCall) (result string, err error) {
	if c.toolCallObserver != nil {
		c.toolCallObserver(call, true, "")
		defer func() {
			c.toolCallObserver(call, false, result)
		}()
	}
	defer func() {
		if c.toolCallLogger == nil {
			return
		}
		if err != nil {
			c.toolCallLogger.Printf("tool call: name=%q arguments=%q error=%q", call.Function.Name, call.Function.Arguments, err.Error())
			return
		}
		c.toolCallLogger.Printf("tool call: name=%q arguments=%q response=%q", call.Function.Name, call.Function.Arguments, result)
	}()

	if c.registry == nil {
		return fmt.Sprintf("%sno tools are available; cannot execute %q", toolErrorPrefix, call.Function.Name), nil
	}

	result, err = c.registry.Execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return toolErrorPrefix + err.Error(), nil
}

func validateToolCall(call ToolCall) error {
	if strings.TrimSpace(call.ID) == "" {
		return errors.New("llm: tool call ID is required")
	}
	if call.Type != "function" {
		return fmt.Errorf("llm: unsupported tool call type %q", call.Type)
	}
	if strings.TrimSpace(call.Function.Name) == "" {
		return fmt.Errorf("llm: tool call %q has no function name", call.ID)
	}
	return nil
}

func validateAssistantMessage(message Message) error {
	if message.Role != "assistant" {
		return fmt.Errorf("llm: completion message role must be assistant, got %q", message.Role)
	}
	if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
		return errors.New("llm: assistant message must contain content or tool calls")
	}

	seenIDs := make(map[string]struct{}, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if err := validateToolCall(call); err != nil {
			return err
		}
		id := strings.TrimSpace(call.ID)
		if _, exists := seenIDs[id]; exists {
			return fmt.Errorf("llm: duplicate tool call ID %q", id)
		}
		seenIDs[id] = struct{}{}
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
	message.PromptAppendTexts = append([]string(nil), message.PromptAppendTexts...)
	message.ProviderMetadata = message.ProviderMetadata.clone()
	return message
}
