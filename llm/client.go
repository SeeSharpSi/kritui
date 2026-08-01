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
	apiKey         string
	model          string
	endpoint       string
	modelsEndpoint string
	responses      bool
	httpClient     *http.Client
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

	path := strings.TrimRight(parsedEndpoint.Path, "/")
	responses := path != "" && strings.HasSuffix(path, "/responses")
	path = strings.TrimSuffix(path, "/chat/completions")
	path = strings.TrimSuffix(path, "/responses")
	parsedEndpoint.Path = strings.TrimRight(path, "/") + "/models"

	return &Client{
		apiKey:         apiKey,
		model:          model,
		endpoint:       endpoint,
		modelsEndpoint: parsedEndpoint.String(),
		responses:      responses,
		httpClient:     &http.Client{},
	}, nil
}

// Complete requests a non-streaming chat completion for messages.
func (c *Client) Complete(ctx context.Context, messages []Message) (Completion, error) {
	return c.complete(ctx, messages, nil)
}

// Models returns model identifiers advertised by the endpoint.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsEndpoint, nil)
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

func (c *Client) postJSON(ctx context.Context, payload any, target any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("llm: encode request: %w", err)
	}
	resp, err := c.post(ctx, encoded)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
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

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(messages))
	for index, message := range messages {
		cloned[index] = append(json.RawMessage(nil), message...)
	}
	return cloned
}
