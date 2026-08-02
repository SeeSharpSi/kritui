package llm

import (
	"context"
	"errors"

	"seesharpsi/kritui/tools"
)

type completionTool struct {
	Type     string           `json:"type"`
	Function tools.Definition `json:"function"`
}

func (c *Client) completeChat(ctx context.Context, messages []Message, definitions []tools.Definition) (Completion, error) {
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
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := c.postJSON(ctx, payload, &response); err != nil {
		return Completion{}, err
	}
	if len(response.Choices) == 0 {
		return Completion{}, errors.New("llm: response contained no choices")
	}

	model := c.completionModel(response.Model)
	message := response.Choices[0].Message
	message.Model = model
	applyUsage(&message, response.Usage)

	return Completion{
		ID:           response.ID,
		Model:        model,
		Message:      message,
		FinishReason: response.Choices[0].FinishReason,
		Usage:        response.Usage,
	}, nil
}
