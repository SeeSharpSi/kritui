package llm

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type headerCaptureTransport struct {
	onRequest func(*http.Request) (*http.Response, error)
	lastReq   *http.Request
}

func (t *headerCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.lastReq = req
	return t.onRequest(req)
}

func jsonCaptureResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGoCompletionSendsSessionHeaderAndUserAgent(t *testing.T) {
	transport := &headerCaptureTransport{
		onRequest: func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("X-OpenCode-Session"); got != "123" {
				t.Errorf("X-OpenCode-Session = %q, want %q", got, "123")
			}
			if got := req.Header.Get("User-Agent"); got != "kritui/1.0" {
				t.Errorf("User-Agent = %q, want %q", got, "kritui/1.0")
			}
			return jsonCaptureResponse(`{
				"id":"completion-1",
				"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]
			}`), nil
		},
	}
	client, err := New("key", "model", "https://opencode.ai/zen/go/v1/chat/completions", ClientOptions{
		SessionID:  "123",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil {
		t.Fatalf("complete() error: %v", err)
	}
	if message.Content != "Hi" {
		t.Errorf("message content = %q, want %q", message.Content, "Hi")
	}
}

func TestGoModelsSendsFallbackSessionHeader(t *testing.T) {
	transport := &headerCaptureTransport{
		onRequest: func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Errorf("method = %q, want GET", req.Method)
			}
			if got := req.Header.Get("X-OpenCode-Session"); got != "models" {
				t.Errorf("X-OpenCode-Session = %q, want %q", got, "models")
			}
			if got := req.Header.Get("User-Agent"); got != "kritui/1.0" {
				t.Errorf("User-Agent = %q, want %q", got, "kritui/1.0")
			}
			return jsonCaptureResponse(`{"data":[{"id":"model-a"}]}`), nil
		},
	}
	client, err := New("key", "model", "https://opencode.ai/zen/go/v1/chat/completions", ClientOptions{
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if len(models) != 1 || models[0] != "model-a" {
		t.Errorf("Models() = %#v, want [model-a]", models)
	}
}

func TestNonGoEndpointOmitsSessionHeader(t *testing.T) {
	transport := &headerCaptureTransport{
		onRequest: func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("X-OpenCode-Session"); got != "" {
				t.Errorf("X-OpenCode-Session = %q, want absent", got)
			}
			if got := req.Header.Get("User-Agent"); got != "kritui/1.0" {
				t.Errorf("User-Agent = %q, want %q", got, "kritui/1.0")
			}
			return jsonCaptureResponse(`{
				"id":"completion-1",
				"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]
			}`), nil
		},
	}
	client, err := New("key", "model", "https://example.com/v1/chat/completions", ClientOptions{
		SessionID:  "123",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if _, err := client.complete(context.Background(), []Message{{Role: "user", Content: "Hello"}}, nil); err != nil {
		t.Fatalf("complete() error: %v", err)
	}
}

func TestIsOpenCodeGoEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "apex go path", endpoint: "https://opencode.ai/zen/go/v1/chat/completions", want: true},
		{name: "subdomain go path", endpoint: "https://zen.opencode.ai/zen/go/v1/chat/completions", want: true},
		{name: "non-opencode host with go path", endpoint: "https://example.com/go/v1/chat/completions", want: false},
		{name: "opencode host without go path", endpoint: "https://opencode.ai/zen/v1/chat/completions", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			if got := isOpenCodeGoEndpoint(parsed); got != test.want {
				t.Errorf("isOpenCodeGoEndpoint(%q) = %v, want %v", test.endpoint, got, test.want)
			}
		})
	}
	if isOpenCodeGoEndpoint(nil) {
		t.Error("isOpenCodeGoEndpoint(nil) = true, want false")
	}
}
