// Package llm provides a client for OpenAI-compatible Chat Completions and
// Responses APIs.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"seesharpsi/kritui/tools"
)

const maxErrorBodySize = 1 << 20

// Message is one message in a chat completion conversation.
type Message struct {
	Role          string     `json:"role"`
	Content       string     `json:"content"`
	Model         string     `json:"-"`
	TotalTokens   *int       `json:"-"`
	Cost          *float64   `json:"-"`
	ToolCalls     []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID    string     `json:"tool_call_id,omitempty"`
	responseItems []json.RawMessage
}

// ToolCall is a function invocation requested by an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall identifies a tool and contains its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage reports token counts returned by the endpoint.
type Usage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	Cost             *float64 `json:"cost,omitempty"`
}

// Completion is the first choice returned by a chat completion request.
type Completion struct {
	ID           string
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// APIError describes a non-successful response from the endpoint.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: endpoint returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Client sends requests to one OpenAI-compatible chat completion endpoint.
type Client struct {
	apiKey     string
	model      string
	endpoint   string
	responses  bool
	httpClient *http.Client
}

// New creates a client. Endpoint must be the full Chat Completions or Responses
// URL; New does not append a path to it. A path ending in /responses selects
// the Responses API; all other paths use Chat Completions.
func New(apiKey, model, endpoint string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("llm: API key is required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("llm: model is required")
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("llm: endpoint is required")
	}

	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("llm: parse endpoint: %w", err)
	}
	if (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") || parsedEndpoint.Host == "" {
		return nil, errors.New("llm: endpoint must be an absolute HTTP or HTTPS URL")
	}
	if parsedEndpoint.Fragment != "" {
		return nil, errors.New("llm: endpoint must not contain a fragment")
	}

	return &Client{
		apiKey:     apiKey,
		model:      model,
		endpoint:   endpoint,
		responses:  strings.TrimRight(parsedEndpoint.Path, "/") != "" && strings.HasSuffix(strings.TrimRight(parsedEndpoint.Path, "/"), "/responses"),
		httpClient: &http.Client{},
	}, nil
}

// Complete requests a non-streaming chat completion for messages.
func (c *Client) Complete(ctx context.Context, messages []Message) (Completion, error) {
	return c.complete(ctx, messages, nil)
}

// Models returns model identifiers advertised by the endpoint.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	modelsURL, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("llm: parse models endpoint: %w", err)
	}
	path := strings.TrimRight(modelsURL.Path, "/")
	path = strings.TrimSuffix(path, "/chat/completions")
	path = strings.TrimSuffix(path, "/responses")
	modelsURL.Path = strings.TrimRight(path, "/") + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("llm: create models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: send models request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, endpointError(resp)
	}

	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("llm: decode models response: %w", err)
	}

	models := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		if model.ID != "" {
			models = append(models, model.ID)
		}
	}
	return models, nil
}

func (c *Client) complete(ctx context.Context, messages []Message, definitions []tools.Definition) (Completion, error) {
	if len(messages) == 0 {
		return Completion{}, errors.New("llm: at least one message is required")
	}
	if c.responses {
		return c.completeResponse(ctx, messages, definitions)
	}
	return c.completeChat(ctx, messages, definitions)
}

func (c *Client) completeChat(ctx context.Context, messages []Message, definitions []tools.Definition) (Completion, error) {
	requestTools := make([]completionTool, len(definitions))
	for index, definition := range definitions {
		requestTools[index] = completionTool{
			Type:     "function",
			Function: definition,
		}
	}

	payload, err := json.Marshal(struct {
		Model    string           `json:"model"`
		Messages []Message        `json:"messages"`
		Tools    []completionTool `json:"tools,omitempty"`
	}{
		Model:    c.model,
		Messages: messages,
		Tools:    requestTools,
	})
	if err != nil {
		return Completion{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := c.post(ctx, payload)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()

	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Completion{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(response.Choices) == 0 {
		return Completion{}, errors.New("llm: response contained no choices")
	}

	message := response.Choices[0].Message
	message.Model = response.Model
	applyUsage(&message, response.Usage)

	return Completion{
		ID:           response.ID,
		Model:        response.Model,
		Message:      message,
		FinishReason: response.Choices[0].FinishReason,
		Usage:        response.Usage,
	}, nil
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

	payload, err := json.Marshal(struct {
		Model string         `json:"model"`
		Input []any          `json:"input"`
		Tools []responseTool `json:"tools,omitempty"`
	}{
		Model: c.model,
		Input: input,
		Tools: requestTools,
	})
	if err != nil {
		return Completion{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := c.post(ctx, payload)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()

	var response struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []json.RawMessage `json:"output"`
		Usage struct {
			InputTokens  int      `json:"input_tokens"`
			OutputTokens int      `json:"output_tokens"`
			TotalTokens  int      `json:"total_tokens"`
			Cost         *float64 `json:"cost,omitempty"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Completion{}, fmt.Errorf("llm: decode response: %w", err)
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

func applyUsage(message *Message, usage Usage) {
	if usage.TotalTokens != 0 || usage.PromptTokens != 0 || usage.CompletionTokens != 0 {
		total := usage.TotalTokens
		message.TotalTokens = &total
	}
	if usage.Cost != nil {
		cost := *usage.Cost
		message.Cost = &cost
	}
}

func (c *Client) post(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: send request: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		return nil, endpointError(resp)
	}
	return resp, nil
}

func endpointError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if err != nil {
		return fmt.Errorf("llm: read error response: %w", err)
	}

	message := strings.TrimSpace(string(body))
	var errorResponse struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errorResponse) == nil && errorResponse.Error.Message != "" {
		message = errorResponse.Error.Message
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}

	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    message,
		Body:       string(body),
	}
}

type completionTool struct {
	Type     string           `json:"type"`
	Function tools.Definition `json:"function"`
}

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

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(messages))
	for index, message := range messages {
		cloned[index] = append(json.RawMessage(nil), message...)
	}
	return cloned
}
