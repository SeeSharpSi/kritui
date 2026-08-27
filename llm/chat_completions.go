package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"seesharpsi/kritui/tools"
)

type completionTool struct {
	Type     string           `json:"type"`
	Function tools.Definition `json:"function"`
}
type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail"`
}
type chatContentPart struct {
	Type     string        `json:"type"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	Text     string        `json:"text,omitempty"`
}

func (c *Client) completeChat(ctx context.Context, endpoint endpointCandidate, messages []Message, definitions []tools.Definition) (Message, error) {
	requestMessages := make([]any, len(messages))
	for i, message := range messages {
		if len(message.Images) == 0 {
			requestMessages[i] = message
			continue
		}
		if message.Role != "user" {
			return Message{}, errors.New("llm: images are only allowed on user messages")
		}
		parts := make([]chatContentPart, 0, len(message.Images)+1)
		for _, image := range message.Images {
			if len(image.Data) == 0 || image.MediaType == "" {
				return Message{}, errors.New("llm: image data and media type are required")
			}
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data), Detail: "auto"}})
		}
		if message.Content != "" {
			parts = append(parts, chatContentPart{Type: "text", Text: message.Content})
		}
		requestMessages[i] = struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		}{message.Role, parts}
	}
	requestTools := make([]completionTool, len(definitions))
	for index, definition := range definitions {
		requestTools[index] = completionTool{
			Type:     "function",
			Function: definition,
		}
	}

	payload := struct {
		Model    string           `json:"model"`
		Messages []any            `json:"messages"`
		Tools    []completionTool `json:"tools,omitempty"`
	}{
		Model:    c.model,
		Messages: requestMessages,
		Tools:    requestTools,
	}
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := c.postJSON(ctx, endpoint, payload, &response); err != nil {
		return Message{}, err
	}
	if len(response.Choices) == 0 {
		return Message{}, errors.New("llm: response contained no choices")
	}

	choice := response.Choices[0]
	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
		if len(choice.Message.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}
	if err := validateChatCompletion(choice.Message, finishReason); err != nil {
		return Message{}, err
	}

	message := choice.Message
	message.Model = c.completionModel(response.Model)
	applyUsage(&message, response.Usage)
	return message, nil
}

func validateChatCompletion(message Message, finishReason string) error {
	if message.Role != "assistant" {
		return fmt.Errorf("llm: completion message role must be assistant, got %q", message.Role)
	}

	switch finishReason {
	case "tool_calls":
		if len(message.ToolCalls) == 0 {
			return errors.New("llm: response finished with tool_calls but contained no tool calls")
		}
	case "stop", "length", "content_filter":
		if len(message.ToolCalls) > 0 {
			return fmt.Errorf("llm: finish reason %q cannot contain tool calls", finishReason)
		}
	default:
		return fmt.Errorf("llm: unsupported finish reason %q", finishReason)
	}

	return validateAssistantMessage(message)
}
