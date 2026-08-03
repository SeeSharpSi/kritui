package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
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

// WebFetchTool fetches web content. Its zero value uses a client that only
// connects to public destinations and is safe for concurrent use.
type WebFetchTool struct {
	HTTPClient *http.Client
}

// NewWebFetchTool creates a webfetch tool using a public-destination-only client.
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

	client := webFetchHTTPClient(t.HTTPClient)

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
	parsedURL, err := url.Parse(params.url)
	if err != nil {
		return webFetchArguments{}, errors.New("webfetch: URL must be a fully formed HTTP or HTTPS URL")
	}
	if err := validateWebFetchURL(parsedURL); err != nil {
		return webFetchArguments{}, err
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

var defaultWebFetchHTTPClient = &http.Client{
	Transport:     secureWebFetchTransport(nil),
	CheckRedirect: checkWebFetchRedirect,
}

func webFetchHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return defaultWebFetchHTTPClient
	}

	secured := *client
	secured.Transport = secureWebFetchTransport(client.Transport)
	redirectPolicy := client.CheckRedirect
	secured.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := checkWebFetchRedirect(request, via); err != nil {
			return err
		}
		if redirectPolicy != nil {
			return redirectPolicy(request, via)
		}
		return nil
	}
	return &secured
}

func secureWebFetchTransport(roundTripper http.RoundTripper) http.RoundTripper {
	var transport *http.Transport
	switch current := roundTripper.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = current.Clone()
	default:
		// Custom round trippers are retained for tests and specialized callers.
		// URL and redirect validation still applies to them.
		return roundTripper
	}

	dialer := &net.Dialer{}
	transport.Proxy = nil
	transport.DialContext = webFetchDialer{
		lookupNetIP: net.DefaultResolver.LookupNetIP,
		dialContext: dialer.DialContext,
	}.DialContext
	transport.DialTLSContext = nil
	return transport
}

func checkWebFetchRedirect(request *http.Request, via []*http.Request) error {
	if err := validateWebFetchURL(request.URL); err != nil {
		return err
	}
	if len(via) >= 10 {
		return errors.New("webfetch: stopped after 10 redirects")
	}
	return nil
}

func validateWebFetchURL(address *url.URL) error {
	if address.Scheme != "http" && address.Scheme != "https" {
		return errors.New("webfetch: URL must start with http:// or https://")
	}
	if address.Host == "" || address.Hostname() == "" {
		return errors.New("webfetch: URL must be a fully formed HTTP or HTTPS URL")
	}
	if address.User != nil {
		return errors.New("webfetch: URL credentials are not allowed")
	}

	port := address.Port()
	if port != "" && (address.Scheme == "http" && port != "80" || address.Scheme == "https" && port != "443") {
		return errors.New("webfetch: URL must use the default port for its scheme")
	}

	host := strings.TrimSuffix(address.Hostname(), ".")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("webfetch: destination must use a public IP address")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !isPublicWebFetchIP(ip) {
		return errors.New("webfetch: destination must use a public IP address")
	}
	return nil
}

type webFetchDialer struct {
	lookupNetIP func(context.Context, string, string) ([]netip.Addr, error)
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func (d webFetchDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("webfetch: parse destination: %w", err)
	}
	addresses, err := d.lookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("webfetch: resolve destination: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("webfetch: destination resolved to no IP addresses")
	}
	for _, ip := range addresses {
		if !isPublicWebFetchIP(ip) {
			return nil, errors.New("webfetch: destination resolved to a non-public IP address")
		}
	}

	var dialErrors []error
	for _, ip := range addresses {
		connection, err := d.dialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, err)
	}
	return nil, fmt.Errorf("webfetch: connect to destination: %w", errors.Join(dialErrors...))
}

var blockedWebFetchPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

var globalWebFetchIPv6Prefix = netip.MustParsePrefix("2000::/3")

func isPublicWebFetchIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() && !globalWebFetchIPv6Prefix.Contains(ip) {
		return false
	}
	for _, prefix := range blockedWebFetchPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
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
