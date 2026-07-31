// Package llm provides a client for OpenAI-compatible chat completion APIs.
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
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Model      string     `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
	httpClient *http.Client
}

// New creates a client. Endpoint must be the full chat completions URL; New
// does not append a path to it.
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Completion{}, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Completion{}, fmt.Errorf("llm: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Completion{}, endpointError(resp)
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
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Completion{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(response.Choices) == 0 {
		return Completion{}, errors.New("llm: response contained no choices")
	}

	message := response.Choices[0].Message
	message.Model = response.Model

	return Completion{
		ID:           response.ID,
		Model:        response.Model,
		Message:      message,
		FinishReason: response.Choices[0].FinishReason,
		Usage:        response.Usage,
	}, nil
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
