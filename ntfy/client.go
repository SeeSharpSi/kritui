package ntfy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxTopicLength        = 64
	defaultRequestTimeout = 5 * time.Second
)

// Config identifies one ntfy publish destination.
type Config struct {
	Endpoint string
	Topic    string
	APIKey   string
}

// HTTPDoer allows publish requests to use a test transport.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client publishes plain-text notifications to ntfy.
type Client struct {
	HTTPClient HTTPDoer
}

// Validate checks destination values before a request is created.
func (c Config) Validate() error {
	endpoint := strings.TrimSpace(c.Endpoint)
	topic := strings.TrimSpace(c.Topic)
	if endpoint == "" {
		return errors.New("ntfy endpoint is required")
	}
	if topic == "" {
		return errors.New("ntfy topic is required")
	}

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("ntfy endpoint must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("ntfy endpoint cannot contain credentials, query, or fragment")
	}
	if len(topic) > maxTopicLength {
		return fmt.Errorf("ntfy topic must be at most %d characters", maxTopicLength)
	}
	for _, character := range topic {
		if !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_') {
			return errors.New("ntfy topic may contain only letters, numbers, hyphens, and underscores")
		}
	}
	return nil
}

// Publish sends one notification. HTTP failures never include response body
// content, preventing remote error text from entering application logs.
func (c Client) Publish(ctx context.Context, config Config, title, message string) error {
	if err := config.Validate(); err != nil {
		return err
	}

	address, err := publishURL(config)
	if err != nil {
		return err
	}
	requestContext := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, address, strings.NewReader(message))
	if err != nil {
		return fmt.Errorf("ntfy: create request: %w", err)
	}
	request.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if title = strings.TrimSpace(title); title != "" {
		request.Header.Set("Title", title)
	}
	if apiKey := strings.TrimSpace(config.APIKey); apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}

	response, err := c.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("ntfy: send request: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("ntfy: endpoint returned HTTP %s", response.Status)
	}
	return nil
}

func publishURL(config Config) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil {
		return "", errors.New("ntfy endpoint is invalid")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + url.PathEscape(strings.TrimSpace(config.Topic))
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (c Client) httpClient() HTTPDoer {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultHTTPClient
}

var defaultHTTPClient = &http.Client{
	Timeout: defaultRequestTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
