package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"seesharpsi/kritui/tools"
)

type responseInput struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
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

func (c *Client) completeResponse(ctx context.Context, messages []Message, definitions []tools.Definition) (Completion, error) {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		if len(message.responseItems) > 0 {
			for _, item := range message.responseItems {
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
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			InputTokens  int      `json:"input_tokens"`
			OutputTokens int      `json:"output_tokens"`
			TotalTokens  int      `json:"total_tokens"`
			Cost         *float64 `json:"cost,omitempty"`
		} `json:"usage"`
	}
	if err := c.postJSON(ctx, payload, &response); err != nil {
		return Completion{}, err
	}

	usage := Usage{
		PromptTokens:     response.Usage.InputTokens,
		CompletionTokens: response.Usage.OutputTokens,
		TotalTokens:      response.Usage.TotalTokens,
		Cost:             response.Usage.Cost,
	}
	message := Message{
		Role:          "assistant",
		Model:         response.Model,
		responseItems: cloneRawMessages(response.Output),
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
			return Completion{}, fmt.Errorf("llm: decode response output: %w", err)
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
		return Completion{}, errors.New("llm: response contained no assistant output")
	}

	finishReason := "stop"
	if len(message.ToolCalls) > 0 {
		finishReason = "tool_calls"
	} else if response.IncompleteDetails.Reason != "" {
		finishReason = response.IncompleteDetails.Reason
	} else if response.Status != "" && response.Status != "completed" {
		finishReason = response.Status
	}

	return Completion{
		ID:           response.ID,
		Model:        response.Model,
		Message:      message,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}
