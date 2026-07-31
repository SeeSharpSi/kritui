package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	webSearchDefaultMaxResults = 8
	webSearchMaxResults        = 20
	webSearchMaxResponseSize   = 2 * 1024 * 1024
	webSearchTimeout           = 30 * time.Second

	webSearchDescription = `Searches the web using a SearXNG server.
Use this tool to find current information and relevant web pages. Results include titles, URLs, snippets, engines, and publication dates when available.`
	webSearchParameters = `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "The web search query"
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "maximum": 20,
      "default": 8,
      "description": "Maximum number of results to return"
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`
)

// WebSearchTool searches a SearXNG server. URL is the server root URL or its
// search endpoint. The tool is safe for concurrent use when HTTPClient is.
type WebSearchTool struct {
	URL        string
	HTTPClient *http.Client
}

// NewWebSearchTool creates a websearch tool for a SearXNG server.
func NewWebSearchTool(serverURL string) *WebSearchTool {
	return &WebSearchTool{URL: serverURL}
}

// Definition describes websearch to an LLM.
func (WebSearchTool) Definition() Definition {
	return Definition{
		Name:        "websearch",
		Description: webSearchDescription,
		Parameters:  json.RawMessage(webSearchParameters),
	}
}

// Execute searches SearXNG and returns compact JSON results.
func (t WebSearchTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	params, err := parseWebSearchArguments(arguments)
	if err != nil {
		return "", err
	}

	endpoint, err := webSearchEndpoint(t.URL, params.query)
	if err != nil {
		return "", err
	}

	requestContext, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("websearch: create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "kritui/1.0")

	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return "", errors.New("websearch: request timed out")
		}
		return "", fmt.Errorf("websearch: send request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("websearch: request returned HTTP %s", response.Status)
	}
	if response.ContentLength > webSearchMaxResponseSize {
		return "", errors.New("websearch: response too large")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, webSearchMaxResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("websearch: read SearXNG response: %w", err)
	}
	if len(body) > webSearchMaxResponseSize {
		return "", errors.New("websearch: response too large")
	}

	var payload searXNGResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("websearch: decode SearXNG response: %w", err)
	}
	if len(payload.Results) > params.maxResults {
		payload.Results = payload.Results[:params.maxResults]
	}

	result := webSearchResponse{
		Query:   payload.Query,
		Results: payload.Results,
	}
	if result.Query == "" {
		result.Query = params.query
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("websearch: encode results: %w", err)
	}
	return string(encoded), nil
}

type webSearchArguments struct {
	query      string
	maxResults int
}

func parseWebSearchArguments(arguments json.RawMessage) (webSearchArguments, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &values); err != nil || values == nil {
		return webSearchArguments{}, errors.New("websearch: arguments must be a JSON object")
	}
	for name := range values {
		if name != "query" && name != "max_results" {
			return webSearchArguments{}, fmt.Errorf("websearch: unknown argument %q", name)
		}
	}

	var params webSearchArguments
	rawQuery, exists := values["query"]
	if !exists || json.Unmarshal(rawQuery, &params.query) != nil {
		return webSearchArguments{}, errors.New("websearch: query must be a string")
	}
	params.query = strings.TrimSpace(params.query)
	if params.query == "" {
		return webSearchArguments{}, errors.New("websearch: query must not be empty")
	}

	params.maxResults = webSearchDefaultMaxResults
	if rawMaxResults, exists := values["max_results"]; exists {
		if err := json.Unmarshal(rawMaxResults, &params.maxResults); err != nil {
			return webSearchArguments{}, errors.New("websearch: max_results must be an integer")
		}
	}
	if params.maxResults < 1 || params.maxResults > webSearchMaxResults {
		return webSearchArguments{}, fmt.Errorf("websearch: max_results must be between 1 and %d", webSearchMaxResults)
	}
	return params, nil
}

func webSearchEndpoint(serverURL, query string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("websearch: URL must be a fully formed HTTP or HTTPS SearXNG URL")
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/search") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/search"
	}
	parameters := parsed.Query()
	parameters.Set("q", query)
	parameters.Set("format", "json")
	parsed.RawQuery = parameters.Encode()
	return parsed.String(), nil
}

type searXNGResponse struct {
	Query   string            `json:"query"`
	Results []webSearchResult `json:"results"`
}

type webSearchResponse struct {
	Query   string            `json:"query"`
	Results []webSearchResult `json:"results"`
}

type webSearchResult struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Content       string   `json:"content,omitempty"`
	Engine        string   `json:"engine,omitempty"`
	Engines       []string `json:"engines,omitempty"`
	PublishedDate string   `json:"publishedDate,omitempty"`
	Score         float64  `json:"score,omitempty"`
}
