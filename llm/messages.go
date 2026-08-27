package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"seesharpsi/kritui/tools"
)

const messagesMaxTokens = 8192

type messagesContentBlock struct {
	Type      string               `json:"type"`
	Text      string               `json:"text,omitempty"`
	ID        string               `json:"id,omitempty"`
	Name      string               `json:"name,omitempty"`
	Input     json.RawMessage      `json:"input,omitempty"`
	ToolUseID string               `json:"tool_use_id,omitempty"`
	Content   string               `json:"content,omitempty"`
	Source    *messagesImageSource `json:"source,omitempty"`
}
type messagesImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type messagesMessage struct {
	Role    string                 `json:"role"`
	Content []messagesContentBlock `json:"content"`
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (c *Client) completeMessages(ctx context.Context, endpoint endpointCandidate, messages []Message, definitions []tools.Definition) (Message, error) {
	system, requestMessages, err := makeMessagesInput(messages)
	if err != nil {
		return Message{}, err
	}
	requestTools := make([]messagesTool, len(definitions))
	for index, definition := range definitions {
		requestTools[index] = messagesTool{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: definition.Parameters,
		}
	}

	payload := struct {
		Model     string            `json:"model"`
		MaxTokens int               `json:"max_tokens"`
		System    string            `json:"system,omitempty"`
		Messages  []messagesMessage `json:"messages"`
		Tools     []messagesTool    `json:"tools,omitempty"`
	}{
		Model:     c.model,
		MaxTokens: messagesMaxTokens,
		System:    system,
		Messages:  requestMessages,
		Tools:     requestTools,
	}
	var response struct {
		Role       string                 `json:"role"`
		Model      string                 `json:"model"`
		Content    []messagesContentBlock `json:"content"`
		StopReason string                 `json:"stop_reason"`
		Usage      struct {
			InputTokens  int      `json:"input_tokens"`
			OutputTokens int      `json:"output_tokens"`
			TotalTokens  int      `json:"total_tokens"`
			Cost         *float64 `json:"cost,omitempty"`
		} `json:"usage"`
	}
	if err := c.postJSON(ctx, endpoint, payload, &response); err != nil {
		return Message{}, err
	}

	message := Message{Role: response.Role, Model: c.completionModel(response.Model)}
	if message.Role == "" {
		message.Role = "assistant"
	}
	for index, block := range response.Content {
		switch block.Type {
		case "text":
			message.Content += block.Text
		case "tool_use":
			arguments := bytes.TrimSpace(block.Input)
			if len(arguments) == 0 || arguments[0] != '{' || !json.Valid(arguments) {
				return Message{}, fmt.Errorf("llm: Messages tool use %d input must be a JSON object", index)
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      block.Name,
					Arguments: string(arguments),
				},
			})
		}
	}
	if err := validateMessagesCompletion(message, response.StopReason); err != nil {
		return Message{}, err
	}
	totalTokens := response.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	}
	applyUsage(&message, Usage{TotalTokens: totalTokens, Cost: response.Usage.Cost})
	return message, nil
}

func makeMessagesInput(messages []Message) (string, []messagesMessage, error) {
	var systemParts []string
	requestMessages := make([]messagesMessage, 0, len(messages))
	for _, message := range messages {
		if len(message.Images) > 0 && message.Role != "user" {
			return "", nil, fmt.Errorf("llm: images are only allowed on user messages")
		}
		var role string
		var content []messagesContentBlock
		switch message.Role {
		case "system", "developer":
			if strings.TrimSpace(message.Content) != "" {
				systemParts = append(systemParts, message.Content)
			}
			continue
		case "user":
			role = "user"
			for _, image := range message.Images {
				if len(image.Data) == 0 || image.MediaType == "" {
					return "", nil, errors.New("llm: image data and media type are required")
				}
				content = append(content, messagesContentBlock{Type: "image", Source: &messagesImageSource{Type: "base64", MediaType: image.MediaType, Data: base64.StdEncoding.EncodeToString(image.Data)}})
			}
			if message.Content != "" {
				content = append(content, messagesContentBlock{Type: "text", Text: message.Content})
			}
		case "assistant":
			role = "assistant"
			if message.Content != "" {
				content = append(content, messagesContentBlock{Type: "text", Text: message.Content})
			}
			for _, call := range message.ToolCalls {
				arguments := bytes.TrimSpace([]byte(call.Function.Arguments))
				if len(arguments) == 0 || arguments[0] != '{' || !json.Valid(arguments) {
					return "", nil, fmt.Errorf("llm: Messages tool call %q arguments must be a JSON object", call.ID)
				}
				content = append(content, messagesContentBlock{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Function.Name,
					Input: append(json.RawMessage(nil), arguments...),
				})
			}
		case "tool":
			role = "user"
			content = append(content, messagesContentBlock{
				Type:      "tool_result",
				ToolUseID: message.ToolCallID,
				Content:   message.Content,
			})
		default:
			return "", nil, fmt.Errorf("llm: Messages API does not support role %q", message.Role)
		}

		if len(requestMessages) > 0 && requestMessages[len(requestMessages)-1].Role == role {
			requestMessages[len(requestMessages)-1].Content = append(requestMessages[len(requestMessages)-1].Content, content...)
		} else {
			requestMessages = append(requestMessages, messagesMessage{Role: role, Content: content})
		}
	}
	if len(requestMessages) == 0 {
		return "", nil, errors.New("llm: Messages request contained no user or assistant messages")
	}
	return strings.Join(systemParts, "\n\n"), requestMessages, nil
}

func validateMessagesCompletion(message Message, stopReason string) error {
	switch stopReason {
	case "tool_use":
		if len(message.ToolCalls) == 0 {
			return errors.New("llm: Messages response stopped for tool_use but contained no tool uses")
		}
	case "":
		// Some compatible providers omit stop_reason. Assistant validation below
		// still requires useful content or a valid tool call.
	case "end_turn", "stop_sequence", "max_tokens", "refusal", "stop":
		if len(message.ToolCalls) > 0 {
			return fmt.Errorf("llm: Messages stop reason %q cannot contain tool uses", stopReason)
		}
	default:
		return fmt.Errorf("llm: unsupported Messages stop reason %q", stopReason)
	}
	return validateAssistantMessage(message)
}
