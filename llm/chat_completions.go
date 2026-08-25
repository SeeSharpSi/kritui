package llm

import (
	"context"
	"errors"
	"fmt"

	"seesharpsi/kritui/tools"
)

type completionTool struct {
	Type     string           `json:"type"`
	Function tools.Definition `json:"function"`
}

func (c *Client) completeChat(ctx context.Context, endpoint endpointCandidate, messages []Message, definitions []tools.Definition) (Message, error) {
	requestTools := make([]completionTool, len(definitions))
	for index, definition := range definitions {
		requestTools[index] = completionTool{
			Type:     "function",
			Function: definition,
		}
	}

	payload := struct {
		Model    string           `json:"model"`
		Messages []Message        `json:"messages"`
		Tools    []completionTool `json:"tools,omitempty"`
	}{
		Model:    c.model,
		Messages: messages,
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
