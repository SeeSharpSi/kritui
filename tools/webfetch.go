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

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"golang.org/x/net/html"
)

const (
	webFetchMaxResponseSize = 5 * 1024 * 1024
	webFetchDefaultTimeout  = 30 * time.Second
	webFetchMaxTimeout      = 120 * time.Second

	webFetchDescription = `Fetches content from a specified URL.
Takes a URL and optional format as input.
Fetches the URL content and converts it to the requested format (markdown by default).
Use this tool to retrieve and analyze web content.

The URL must be a fully formed HTTP or HTTPS URL. Supported formats are markdown, text, and html. This tool is read-only and does not modify files.`
	webFetchParameters = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL to fetch content from"
    },
    "format": {
      "type": "string",
      "enum": ["text", "markdown", "html"],
      "description": "The format to return the content in (text, markdown, or html). Defaults to markdown.",
      "default": "markdown"
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

var webFetchAcceptHeaders = map[string]string{
	"markdown": "text/markdown;q=1.0, text/x-markdown;q=0.9, text/plain;q=0.8, text/html;q=0.7, */*;q=0.1",
	"text":     "text/plain;q=1.0, text/markdown;q=0.9, text/html;q=0.8, */*;q=0.1",
	"html":     "text/html;q=1.0, application/xhtml+xml;q=0.9, text/plain;q=0.8, text/markdown;q=0.7, */*;q=0.1",
}

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

// Execute fetches a URL and returns its content in the requested format.
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

	response, err := fetchWebResponse(requestContext, client, params.url, params.format, webFetchBrowserUserAgent)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return "", errors.New("webfetch: request timed out")
		}
		return "", fmt.Errorf("webfetch: send request: %w", err)
	}

	if response.StatusCode == http.StatusForbidden && response.Header.Get("cf-mitigated") == "challenge" {
		response.Body.Close()
		response, err = fetchWebResponse(requestContext, client, params.url, params.format, "opencode")
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded) {
				return "", errors.New("webfetch: request timed out")
			}
			return "", fmt.Errorf("webfetch: send retry: %w", err)
		}
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
		return "Image fetched successfully", nil
	}

	result := strings.ToValidUTF8(string(content), "\uFFFD")
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		switch params.format {
		case "markdown":
			markdown, err := htmltomarkdown.ConvertString(result)
			if err != nil {
				return "", fmt.Errorf("webfetch: convert HTML to markdown: %w", err)
			}
			return markdown, nil
		case "text":
			text, err := extractTextFromHTML(result)
			if err != nil {
				return "", fmt.Errorf("webfetch: extract HTML text: %w", err)
			}
			return text, nil
		}
	}
	return result, nil
}

const webFetchBrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"

type webFetchArguments struct {
	url     string
	format  string
	timeout *float64
}

func parseWebFetchArguments(arguments json.RawMessage) (webFetchArguments, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &values); err != nil || values == nil {
		return webFetchArguments{}, errors.New("webfetch: arguments must be a JSON object")
	}
	for name := range values {
		if name != "url" && name != "format" && name != "timeout" {
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

	params.format = "markdown"
	if rawFormat, exists := values["format"]; exists {
		if err := json.Unmarshal(rawFormat, &params.format); err != nil {
			return webFetchArguments{}, errors.New("webfetch: format must be a string")
		}
	}
	if params.format != "markdown" && params.format != "text" && params.format != "html" {
		return webFetchArguments{}, errors.New(`webfetch: format must be "markdown", "text", or "html"`)
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

func fetchWebResponse(ctx context.Context, client *http.Client, address, format, userAgent string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", webFetchAcceptHeaders[format])
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return client.Do(request)
}

func extractTextFromHTML(source string) (string, error) {
	root, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", err
	}
	var output strings.Builder
	writeHTMLText(&output, root)
	return strings.TrimSpace(output.String()), nil
}

func writeHTMLText(output *strings.Builder, node *html.Node) {
	if node.Type == html.ElementNode && shouldSkipHTMLText(node.Data) {
		return
	}
	if node.Type == html.TextNode {
		output.WriteString(node.Data)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		writeHTMLText(output, child)
	}
}

func shouldSkipHTMLText(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "iframe", "object", "embed":
		return true
	default:
		return false
	}
}
