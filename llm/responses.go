package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"seesharpsi/kritui/tools"
)

type responseInput struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (c *Client) completeResponse(ctx context.Context, endpoint endpointCandidate, messages []Message, definitions []tools.Definition) (Message, error) {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		if responseOutput := message.ProviderMetadata.ResponsesOutput(); len(responseOutput) > 0 {
			for _, item := range responseOutput {
				input = append(input, item)
			}
			continue
		}
		switch message.Role {
		case "tool":
			input = append(input, responseInput{
				Type:   "function_call_output",
				CallID: message.ToolCallID,
				Output: message.Content,
			})
		default:
			if len(message.Images) > 0 {
				if message.Role != "user" {
					return Message{}, errors.New("llm: images are only allowed on user messages")
				}
				parts := make([]any, 0, len(message.Images)+1)
				for _, image := range message.Images {
					if len(image.Data) == 0 || image.MediaType == "" {
						return Message{}, errors.New("llm: image data and media type are required")
					}
					parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data), "detail": "auto"})
				}
				if message.Content != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": message.Content})
				}
				input = append(input, responseInput{Role: message.Role, Content: parts})
				continue
			}
			if message.Content != "" || len(message.ToolCalls) == 0 {
				input = append(input, responseInput{Role: message.Role, Content: message.Content})
			}
			for _, call := range message.ToolCalls {
				input = append(input, responseInput{
					Type:      "function_call",
					CallID:    call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				})
			}
		}
	}

	requestTools := make([]responseTool, len(definitions))
	for index, definition := range definitions {
		requestTools[index] = responseTool{
			Type:        "function",
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.Parameters,
		}
	}

	payload := struct {
		Model string         `json:"model"`
		Input []any          `json:"input"`
		Tools []responseTool `json:"tools,omitempty"`
	}{
		Model: c.model,
		Input: input,
		Tools: requestTools,
	}
	var response struct {
		Model  string            `json:"model"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			TotalTokens int      `json:"total_tokens"`
			Cost        *float64 `json:"cost,omitempty"`
		} `json:"usage"`
	}
	if err := c.postJSON(ctx, endpoint, payload, &response); err != nil {
		return Message{}, err
	}

	usage := Usage{
		TotalTokens: response.Usage.TotalTokens,
		Cost:        response.Usage.Cost,
	}
	model := c.completionModel(response.Model)
	providerMetadata, err := NewResponsesProviderMetadata(response.Output)
	if err != nil {
		return Message{}, err
	}
	message := Message{
		Role:             "assistant",
		Model:            model,
		ProviderMetadata: providerMetadata,
	}
	applyUsage(&message, usage)
	for _, rawOutput := range response.Output {
		var output struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rawOutput, &output); err != nil {
			return Message{}, fmt.Errorf("llm: decode response output: %w", err)
		}
		switch output.Type {
		case "message":
			if output.Role != "" {
				message.Role = output.Role
			}
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					message.Content += content.Text
				case "refusal":
					message.Content += content.Refusal
				}
			}
		case "function_call":
			message.ToolCalls = append(message.ToolCalls, ToolCall{
				ID:   output.CallID,
				Type: "function",
				Function: FunctionCall{
					Name:      output.Name,
					Arguments: output.Arguments,
				},
			})
		}
	}
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return Message{}, errors.New("llm: response contained no assistant output")
	}
	if err := validateAssistantMessage(message); err != nil {
		return Message{}, err
	}
	return message, nil
}
