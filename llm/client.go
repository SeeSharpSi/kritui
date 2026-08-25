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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"seesharpsi/kritui/tools"
)

const (
	maxErrorBodySize           = 1 << 20
	defaultMaxResponseBodySize = 16 << 20
	defaultModelsTimeout       = 5 * time.Second
	defaultCompletionTimeout   = 10 * time.Minute
	providerConnectTimeout     = 10 * time.Second
)

// EndpointType identifies a provider request and response protocol.
type EndpointType string

const (
	EndpointResponses       EndpointType = "responses"
	EndpointMessages        EndpointType = "messages"
	EndpointChatCompletions EndpointType = "chat_completions"
)

type endpointCandidate struct {
	typeName EndpointType
	url      string
}

var endpointDefinitions = []struct {
	typeName EndpointType
	suffix   string
}{
	{typeName: EndpointResponses, suffix: "/responses"},
	{typeName: EndpointMessages, suffix: "/chat/messages"},
	{typeName: EndpointChatCompletions, suffix: "/chat/completions"},
}

// Message is one message in a chat completion conversation.
type Message struct {
	// ID is local storage identity and is never sent to providers.
	ID          int64    `json:"-"`
	Role        string   `json:"role"`
	Content     string   `json:"content"`
	Model       string   `json:"-"`
	TotalTokens *int     `json:"-"`
	Cost        *float64 `json:"-"`
	// PromptAppendTexts contains snapshotted append text expanded before
	// provider requests.
	PromptAppendTexts []string         `json:"-"`
	ToolCalls         []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID        string           `json:"tool_call_id,omitempty"`
	ProviderMetadata  ProviderMetadata `json:"-"`
}

// ProviderMetadata retains provider-specific state needed for later requests.
// Its zero value contains no metadata.
type ProviderMetadata struct {
	responsesOutput []json.RawMessage
}

type storedProviderMetadata struct {
	ResponsesOutput []json.RawMessage `json:"responses_output"`
}

// NewResponsesProviderMetadata validates provider output items and returns
// metadata suitable for restoring a Responses API conversation.
func NewResponsesProviderMetadata(output []json.RawMessage) (ProviderMetadata, error) {
	if err := validateResponsesOutput(output); err != nil {
		return ProviderMetadata{}, err
	}
	return ProviderMetadata{responsesOutput: cloneRawMessages(output)}, nil
}

// ResponsesOutput returns a deep copy of stored Responses API output items.
func (m ProviderMetadata) ResponsesOutput() []json.RawMessage {
	return cloneRawMessages(m.responsesOutput)
}

// IsZero reports whether metadata contains no provider state.
func (m ProviderMetadata) IsZero() bool {
	return len(m.responsesOutput) == 0
}

// MarshalJSON encodes validated provider metadata for durable storage.
func (m ProviderMetadata) MarshalJSON() ([]byte, error) {
	if err := validateResponsesOutput(m.responsesOutput); err != nil {
		return nil, err
	}
	return json.Marshal(storedProviderMetadata{ResponsesOutput: m.responsesOutput})
}

// UnmarshalJSON restores and validates provider metadata from durable storage.
func (m *ProviderMetadata) UnmarshalJSON(data []byte) error {
	if m == nil {
		return errors.New("llm: provider metadata destination is nil")
	}
	var encoded struct {
		ResponsesOutput json.RawMessage `json:"responses_output"`
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		return fmt.Errorf("llm: decode provider metadata: %w", err)
	}
	if len(encoded.ResponsesOutput) == 0 || bytes.Equal(bytes.TrimSpace(encoded.ResponsesOutput), []byte("null")) {
		return errors.New("llm: provider metadata responses_output must be a non-empty JSON array")
	}
	var stored storedProviderMetadata
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("llm: decode provider metadata: %w", err)
	}
	if err := validateResponsesOutput(stored.ResponsesOutput); err != nil {
		return err
	}
	m.responsesOutput = cloneRawMessages(stored.ResponsesOutput)
	return nil
}

func (m ProviderMetadata) clone() ProviderMetadata {
	return ProviderMetadata{responsesOutput: cloneRawMessages(m.responsesOutput)}
}

func validateResponsesOutput(output []json.RawMessage) error {
	if len(output) == 0 {
		return errors.New("llm: provider metadata responses_output must contain at least one item")
	}
	for index, raw := range output {
		var item struct {
			Type string `json:"type"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &item) != nil {
			return fmt.Errorf("llm: Responses output item %d must be a JSON object", index)
		}
		if strings.TrimSpace(item.Type) == "" {
			return fmt.Errorf("llm: Responses output item %d has no type", index)
		}
	}
	return nil
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

// Usage reports the persisted token accounting returned by the endpoint.
type Usage struct {
	TotalTokens int      `json:"total_tokens"`
	Cost        *float64 `json:"cost,omitempty"`
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

// Client sends requests to OpenAI-compatible Responses, Anthropic Messages,
// and Chat Completions endpoints. It is safe for concurrent use.
type Client struct {
	apiKey              string
	model               string
	endpoints           []endpointCandidate
	configuredEndpoint  EndpointType
	modelsEndpoint      string
	httpClient          *http.Client
	modelsTimeout       time.Duration
	completeTimeout     time.Duration
	maxResponseBodySize int64
	endpointSelected    func(EndpointType)
	endpointMu          sync.RWMutex
	preferredEndpoint   EndpointType
	selectedEndpoint    EndpointType
}

// ClientOptions configures provider request deadlines and response limits.
// Zero values use secure defaults.
type ClientOptions struct {
	HTTPClient          *http.Client
	ModelsTimeout       time.Duration
	CompletionTimeout   time.Duration
	MaxResponseBodySize int64
	// PreferredEndpoint is attempted first when the configured URL has a
	// recognized endpoint suffix.
	PreferredEndpoint EndpointType
	// EndpointSelected runs after a protocol produces a valid completion and
	// differs from PreferredEndpoint. It must be safe for synchronous use.
	EndpointSelected func(EndpointType)
}

// New creates a client. Endpoint must be a full Responses, Messages, or Chat
// Completions URL. Recognized suffixes enable HTTP-500 fallback through sibling
// protocol URLs; custom paths retain their previous Chat Completions behavior.
func New(apiKey, model, endpoint string, options ...ClientOptions) (*Client, error) {
	if len(options) > 1 {
		return nil, errors.New("llm: at most one client options value is allowed")
	}
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

	endpoints, configuredEndpoint, modelsEndpoint := configureEndpoints(parsedEndpoint, endpoint)

	configuration := ClientOptions{}
	if len(options) == 1 {
		configuration = options[0]
	}
	if configuration.ModelsTimeout < 0 {
		return nil, errors.New("llm: models timeout must not be negative")
	}
	if configuration.CompletionTimeout < 0 {
		return nil, errors.New("llm: completion timeout must not be negative")
	}
	if configuration.MaxResponseBodySize < 0 {
		return nil, errors.New("llm: maximum response body size must not be negative")
	}
	if configuration.PreferredEndpoint != "" && !validEndpointType(configuration.PreferredEndpoint) {
		return nil, fmt.Errorf("llm: unsupported preferred endpoint type %q", configuration.PreferredEndpoint)
	}
	if configuration.ModelsTimeout == 0 {
		configuration.ModelsTimeout = defaultModelsTimeout
	}
	if configuration.CompletionTimeout == 0 {
		configuration.CompletionTimeout = defaultCompletionTimeout
	}
	if configuration.MaxResponseBodySize == 0 {
		configuration.MaxResponseBodySize = defaultMaxResponseBodySize
	}
	if configuration.HTTPClient == nil {
		configuration.HTTPClient = defaultHTTPClient()
	}

	return &Client{
		apiKey:              apiKey,
		model:               model,
		endpoints:           endpoints,
		configuredEndpoint:  configuredEndpoint,
		modelsEndpoint:      modelsEndpoint,
		httpClient:          configuration.HTTPClient,
		modelsTimeout:       configuration.ModelsTimeout,
		completeTimeout:     configuration.CompletionTimeout,
		maxResponseBodySize: configuration.MaxResponseBodySize,
		endpointSelected:    configuration.EndpointSelected,
		preferredEndpoint:   configuration.PreferredEndpoint,
	}, nil
}

func (c *Client) complete(ctx context.Context, messages []Message, definitions []tools.Definition) (Message, error) {
	if len(messages) == 0 {
		return Message{}, errors.New("llm: at least one message is required")
	}
	ctx, cancel := context.WithTimeout(ctx, c.completeTimeout)
	defer cancel()

	endpoints := c.orderedEndpoints()
	for index, endpoint := range endpoints {
		var message Message
		var err error
		switch endpoint.typeName {
		case EndpointResponses:
			message, err = c.completeResponse(ctx, endpoint, messages, definitions)
		case EndpointMessages:
			message, err = c.completeMessages(ctx, endpoint, messages, definitions)
		case EndpointChatCompletions:
			message, err = c.completeChat(ctx, endpoint, messages, definitions)
		default:
			return Message{}, fmt.Errorf("llm: unsupported endpoint type %q", endpoint.typeName)
		}
		if err == nil {
			c.rememberEndpoint(endpoint.typeName)
			return message, nil
		}
		if index == len(endpoints)-1 || !isHTTP500(err) {
			return Message{}, err
		}
	}
	return Message{}, errors.New("llm: no completion endpoints are configured")
}

func (c *Client) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.modelsTimeout)
	defer cancel()

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
	if err := decodeJSONBody(resp.Body, c.maxResponseBodySize, &response); err != nil {
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

func (c *Client) postJSON(ctx context.Context, endpoint endpointCandidate, payload any, target any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("llm: encode request: %w", err)
	}
	resp, err := c.post(ctx, endpoint, encoded)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := decodeJSONBody(resp.Body, c.maxResponseBodySize, target); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, endpoint endpointCandidate, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if endpoint.typeName == EndpointMessages {
		req.Header.Set("X-Api-Key", c.apiKey)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}

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

func configureEndpoints(parsedEndpoint *url.URL, configuredURL string) ([]endpointCandidate, EndpointType, string) {
	path := strings.TrimRight(parsedEndpoint.Path, "/")
	configuredType := EndpointChatCompletions
	basePath := path
	recognized := false

	aliases := []struct {
		typeName EndpointType
		suffix   string
	}{
		{typeName: EndpointChatCompletions, suffix: "/chat/completions"},
		{typeName: EndpointMessages, suffix: "/chat/messages"},
		{typeName: EndpointResponses, suffix: "/responses"},
		{typeName: EndpointMessages, suffix: "/messages"},
	}
	for _, alias := range aliases {
		if strings.HasSuffix(path, alias.suffix) {
			configuredType = alias.typeName
			basePath = strings.TrimSuffix(path, alias.suffix)
			recognized = true
			break
		}
	}

	modelsURL := *parsedEndpoint
	if recognized {
		modelsURL.Path = strings.TrimRight(basePath, "/") + "/models"
	} else {
		modelsURL.Path = strings.TrimRight(path, "/") + "/models"
	}
	modelsURL.RawPath = ""

	if !recognized {
		return []endpointCandidate{{typeName: configuredType, url: configuredURL}}, configuredType, modelsURL.String()
	}

	endpoints := make([]endpointCandidate, 0, len(endpointDefinitions))
	for _, definition := range endpointDefinitions {
		endpointURL := *parsedEndpoint
		endpointURL.Path = strings.TrimRight(basePath, "/") + definition.suffix
		endpointURL.RawPath = ""
		value := endpointURL.String()
		if definition.typeName == configuredType {
			value = configuredURL
		}
		endpoints = append(endpoints, endpointCandidate{typeName: definition.typeName, url: value})
	}
	return endpoints, configuredType, modelsURL.String()
}

func validEndpointType(endpointType EndpointType) bool {
	switch endpointType {
	case EndpointResponses, EndpointMessages, EndpointChatCompletions:
		return true
	default:
		return false
	}
}

func (c *Client) orderedEndpoints() []endpointCandidate {
	c.endpointMu.RLock()
	start := c.selectedEndpoint
	preferred := c.preferredEndpoint
	c.endpointMu.RUnlock()
	if start == "" {
		start = preferred
	}
	if !c.hasEndpoint(start) {
		start = c.configuredEndpoint
	}

	startIndex := 0
	for index, definition := range endpointDefinitions {
		if definition.typeName == start {
			startIndex = index
			break
		}
	}
	ordered := make([]endpointCandidate, 0, len(c.endpoints))
	for offset := range endpointDefinitions {
		typeName := endpointDefinitions[(startIndex+offset)%len(endpointDefinitions)].typeName
		for _, endpoint := range c.endpoints {
			if endpoint.typeName == typeName {
				ordered = append(ordered, endpoint)
				break
			}
		}
	}
	return ordered
}

func (c *Client) hasEndpoint(endpointType EndpointType) bool {
	for _, endpoint := range c.endpoints {
		if endpoint.typeName == endpointType {
			return true
		}
	}
	return false
}

func (c *Client) rememberEndpoint(endpointType EndpointType) {
	c.endpointMu.Lock()
	c.selectedEndpoint = endpointType
	changed := c.preferredEndpoint != endpointType
	if changed {
		c.preferredEndpoint = endpointType
	}
	callback := c.endpointSelected
	c.endpointMu.Unlock()
	if changed && callback != nil {
		callback(endpointType)
	}
}

func isHTTP500(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusInternalServerError
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   providerConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

func decodeJSONBody(body io.Reader, limit int64, target any) error {
	limited := &io.LimitedReader{R: body, N: limit}
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if limited.N == 0 {
		extra, err := io.ReadAll(io.LimitReader(body, 1))
		if err != nil {
			return err
		}
		if len(extra) != 0 {
			return fmt.Errorf("response exceeds %d bytes", limit)
		}
	}
	return json.Unmarshal(data, target)
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
	if usage.TotalTokens != 0 {
		total := usage.TotalTokens
		message.TotalTokens = &total
	}
	if usage.Cost != nil {
		cost := *usage.Cost
		message.Cost = &cost
	}
}

func (c *Client) completionModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return c.model
	}
	return model
}

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(messages))
	for index, message := range messages {
		cloned[index] = append(json.RawMessage(nil), message...)
	}
	return cloned
}
