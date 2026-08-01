package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

const (
	webFetchMaxResponseSize = 5 * 1024 * 1024
	webFetchDefaultTimeout  = 30 * time.Second
	webFetchMaxTimeout      = 120 * time.Second
	webFetchDefaultLimit    = 8000
	webFetchMaxLimit        = 32000

	webFetchAccept = "text/markdown;q=1.0, text/x-markdown;q=0.9, text/plain;q=0.8, text/html;q=0.7, */*;q=0.1"

	webFetchDescription = `Fetches content from a specified URL and returns compact JSON.
HTML pages are converted to markdown. Use offset and limit to page through long content.
The URL must be a fully formed HTTP or HTTPS URL. This tool is read-only and does not modify files.`
	webFetchParameters = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL to fetch content from"
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "default": 0,
      "description": "Character offset into the converted content"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 32000,
      "default": 8000,
      "description": "Maximum number of characters to return"
    },
    "timeout": {
      "type": "number",
      "description": "Optional timeout in seconds (max 120)"
    }
  },
  "required": ["url"],
  "additionalProperties": false
}`
)

// WebFetchTool fetches web content. Its zero value uses http.DefaultClient and
// is safe for concurrent use when its HTTPClient is safe for concurrent use.
type WebFetchTool struct {
	HTTPClient *http.Client
}

// NewWebFetchTool creates a webfetch tool using http.DefaultClient.
func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{}
}

// Definition describes webfetch to an LLM.
func (WebFetchTool) Definition() Definition {
	return Definition{
		Name:        "webfetch",
		Description: webFetchDescription,
		Parameters:  json.RawMessage(webFetchParameters),
	}
}

// Execute fetches a URL and returns a JSON slice of its content.
func (t WebFetchTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	params, err := parseWebFetchArguments(arguments)
	if err != nil {
		return "", err
	}

	timeout := webFetchDefaultTimeout
	if params.timeout != nil {
		seconds := *params.timeout
		if seconds > webFetchMaxTimeout.Seconds() {
			seconds = webFetchMaxTimeout.Seconds()
		}
		timeout = time.Duration(seconds * float64(time.Second))
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	operation := "send request"
	response, err := fetchWebResponse(requestContext, client, params.url, webFetchBrowserUserAgent)
	if err == nil && response.StatusCode == http.StatusForbidden && response.Header.Get("cf-mitigated") == "challenge" {
		response.Body.Close()
		operation = "send retry"
		response, err = fetchWebResponse(requestContext, client, params.url, "opencode")
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return "", errors.New("webfetch: request timed out")
		}
		return "", fmt.Errorf("webfetch: %s: %w", operation, err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("webfetch: request returned HTTP %s", response.Status)
	}
	if response.ContentLength > webFetchMaxResponseSize {
		return "", errors.New("webfetch: response too large (exceeds 5MB limit)")
	}
	if contentLength := response.Header.Get("Content-Length"); contentLength != "" {
		if size, parseErr := strconv.ParseInt(contentLength, 10, 64); parseErr == nil && size > webFetchMaxResponseSize {
			return "", errors.New("webfetch: response too large (exceeds 5MB limit)")
		}
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, webFetchMaxResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("webfetch: read response: %w", err)
	}
	if len(content) > webFetchMaxResponseSize {
		return "", errors.New("webfetch: response too large (exceeds 5MB limit)")
	}

	contentType := response.Header.Get("Content-Type")
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml" && mimeType != "image/vnd.fastbidsheet" {
		return encodeWebFetchResponse(webFetchResponse{
			URL:     params.url,
			Content: "Image fetched successfully",
			Offset:  0,
			Limit:   params.limit,
			Total:   0,
		})
	}

	body := strings.ToValidUTF8(string(content), "\uFFFD")
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		markdown, err := htmltomarkdown.ConvertString(body)
		if err != nil {
			return "", fmt.Errorf("webfetch: convert HTML to markdown: %w", err)
		}
		body = markdown
	}

	sliced, total := sliceRunes(body, params.offset, params.limit)
	return encodeWebFetchResponse(webFetchResponse{
		URL:       params.url,
		Content:   sliced,
		Offset:    params.offset,
		Limit:     params.limit,
		Total:     total,
		Truncated: params.offset+utf8.RuneCountInString(sliced) < total,
	})
}

const webFetchBrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"

type webFetchArguments struct {
	url     string
	offset  int
	limit   int
	timeout *float64
}

type webFetchResponse struct {
	URL       string `json:"url"`
	Content   string `json:"content"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated,omitempty"`
}

func parseWebFetchArguments(arguments json.RawMessage) (webFetchArguments, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &values); err != nil || values == nil {
		return webFetchArguments{}, errors.New("webfetch: arguments must be a JSON object")
	}
	for name := range values {
		switch name {
		case "url", "offset", "limit", "timeout":
		default:
			return webFetchArguments{}, fmt.Errorf("webfetch: unknown argument %q", name)
		}
	}

	var params webFetchArguments
	rawURL, exists := values["url"]
	if !exists || json.Unmarshal(rawURL, &params.url) != nil {
		return webFetchArguments{}, errors.New("webfetch: url must be a string")
	}
	if !strings.HasPrefix(params.url, "http://") && !strings.HasPrefix(params.url, "https://") {
		return webFetchArguments{}, errors.New("webfetch: URL must start with http:// or https://")
	}
	parsedURL, err := url.Parse(params.url)
	if err != nil || parsedURL.Host == "" {
		return webFetchArguments{}, errors.New("webfetch: URL must be a fully formed HTTP or HTTPS URL")
	}

	params.offset = 0
	if rawOffset, exists := values["offset"]; exists {
		if err := json.Unmarshal(rawOffset, &params.offset); err != nil {
			return webFetchArguments{}, errors.New("webfetch: offset must be an integer")
		}
	}
	if params.offset < 0 {
		return webFetchArguments{}, errors.New("webfetch: offset must be >= 0")
	}

	params.limit = webFetchDefaultLimit
	if rawLimit, exists := values["limit"]; exists {
		if err := json.Unmarshal(rawLimit, &params.limit); err != nil {
			return webFetchArguments{}, errors.New("webfetch: limit must be an integer")
		}
	}
	if params.limit < 1 || params.limit > webFetchMaxLimit {
		return webFetchArguments{}, fmt.Errorf("webfetch: limit must be between 1 and %d", webFetchMaxLimit)
	}

	if rawTimeout, exists := values["timeout"]; exists {
		var timeout float64
		if string(rawTimeout) == "null" || json.Unmarshal(rawTimeout, &timeout) != nil {
			return webFetchArguments{}, errors.New("webfetch: timeout must be a number")
		}
		params.timeout = &timeout
	}

	return params, nil
}

func fetchWebResponse(ctx context.Context, client *http.Client, address, userAgent string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", webFetchAccept)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return client.Do(request)
}

func sliceRunes(content string, offset, limit int) (string, int) {
	runes := []rune(content)
	total := len(runes)
	if offset >= total {
		return "", total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return string(runes[offset:end]), total
}

func encodeWebFetchResponse(result webFetchResponse) (string, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("webfetch: encode response: %w", err)
	}
	return string(encoded), nil
}
