package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func TestCompleteFallsBackOnStructuredUnsupportedProtocol400AndRemembersWinner(t *testing.T) {
	var paths []string
	var selected []EndpointType
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/v1/chat/messages?api-version=1", ClientOptions{
		EndpointSelected: func(endpoint EndpointType) { selected = append(selected, endpoint) },
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	for range 2 {
		message, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
		if err != nil {
			t.Fatalf("complete() error: %v", err)
		}
		if message.Role != "assistant" {
			t.Errorf("message role = %q, want assistant", message.Role)
		}
	}

	wantPaths := []string{
		"/v1/chat/messages?api-version=1",
		"/v1/chat/completions?api-version=1",
		"/v1/responses?api-version=1",
		"/v1/responses?api-version=1",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("request paths = %v, want %v", paths, wantPaths)
	}
	if !slices.Equal(selected, []EndpointType{EndpointResponses}) {
		t.Errorf("selected endpoint callbacks = %v, want Responses once", selected)
	}
}

func TestCompleteDoesNotFallbackOnUnrelatedErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unrelated bad request", status: http.StatusBadRequest, body: `{"error":{"message":"unsupported protocol"}}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"code":"ModelProtocolUnsupported","message":"unsupported protocol"}}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"type":"ModelProtocolUnsupported","message":"unsupported protocol"}}`},
		{name: "not found", status: http.StatusNotFound, body: `{"error":{"message":"unsupported protocol"}}`},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":{"message":"unsupported protocol"}}`},
		{name: "bad gateway", status: http.StatusBadGateway, body: `{"error":{"message":"unsupported protocol"}}`},
		{name: "numeric OpenCode code", status: http.StatusBadRequest, body: `{"type":"error","error":{"code":1234,"message":"unsupported protocol"}}`},
		{name: "unknown OpenCode code", status: http.StatusBadRequest, body: `{"type":"error","error":{"code":"UnknownProviderError","message":"unsupported protocol"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL+"/v1/responses")
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != test.status {
				t.Fatalf("complete() error = %v, want APIError status %d", err, test.status)
			}
			if apiError.Message == "" {
				t.Errorf("APIError message is empty; structured error message extraction failed")
			}
			if !slices.Equal(paths, []string{"/v1/responses"}) {
				t.Errorf("request paths = %v, want no fallback", paths)
			}
		})
	}
}

func TestCompleteDoesNotFallbackForCustomPath(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"ModelProtocolUnsupported"}}`)
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/custom/endpoint")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
	if err == nil {
		t.Fatal("complete() error = nil, want APIError")
	}
	if !slices.Equal(paths, []string{"/custom/endpoint"}) {
		t.Errorf("request paths = %v, want single custom endpoint request", paths)
	}
}

func TestStructuredUnsupportedProtocolIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "error code", body: `{"error":{"code":"ModelProtocolUnsupported"}}`, want: true},
		{name: "error type", body: `{"error":{"type":"modelprotocolunsupported"}}`, want: true},
		{name: "top-level code", body: `{"code":"unsupported_protocol"}`, want: true},
		{name: "top-level type", body: `{"type":"protocol-not-supported"}`, want: true},
		{name: "top-level name", body: `{"name":"ModelProtocolUnsupported"}`, want: true},
		{name: "error name", body: `{"error":{"name":"ModelProtocolUnsupported"}}`, want: true},
		{name: "numeric code with recognized type", body: `{"error":{"code":1234,"type":"ModelProtocolUnsupported"}}`, want: true},
		{name: "echoed request context", body: `{"error":{"type":"invalid_request_error","details":{"request":{"type":"ModelProtocolUnsupported"}}}}`},
		{name: "prose only", body: `{"error":{"message":"unsupported protocol"}}`},
		{name: "numeric identifier", body: `{"error":{"code":1234,"message":"unsupported protocol"}}`},
		{name: "unknown identifier", body: `{"error":{"code":"OtherError","message":"unsupported protocol"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isUnsupportedProtocolError(test.body); got != test.want {
				t.Errorf("isUnsupportedProtocolError() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMessagesAlternateRouteRetriesOnlyOn404Or405AndRemembersWinner(t *testing.T) {
	tests := []struct {
		name           string
		configuredPath string
		primaryPath    string
		alternatePath  string
		status         int
	}{
		{name: "chat messages primary", configuredPath: "/v1/chat/messages", primaryPath: "/v1/chat/messages", alternatePath: "/v1/messages", status: http.StatusNotFound},
		{name: "messages primary", configuredPath: "/v1/messages", primaryPath: "/v1/messages", alternatePath: "/v1/chat/messages", status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.URL.Path == test.primaryPath {
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, `{"error":{"message":"route unavailable"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"role":"assistant","model":"model","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL+test.configuredPath+"?tenant=a%2Fb&region=eu")
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			for range 2 {
				if _, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil); err != nil {
					t.Fatalf("complete() error: %v", err)
				}
			}
			wantPaths := []string{
				test.primaryPath + "?tenant=a%2Fb&region=eu",
				test.alternatePath + "?tenant=a%2Fb&region=eu",
				test.alternatePath + "?tenant=a%2Fb&region=eu",
			}
			if !slices.Equal(paths, wantPaths) {
				t.Errorf("request paths = %v, want %v", paths, wantPaths)
			}
		})
	}
}

func TestMessagesAlternateRouteDoesNotRetryOtherStatuses(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"unsupported protocol"}}`)
			}))
			defer server.Close()

			client, err := New("key", "model", server.URL+"/v1/chat/messages")
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
			if err == nil {
				t.Fatal("complete() error = nil, want provider error")
			}
			if len(paths) == 0 || paths[0] != "/v1/chat/messages" {
				t.Fatalf("first request paths = %v, want Messages primary route", paths)
			}
			if slices.Contains(paths, "/v1/messages") {
				t.Errorf("request paths = %v, unexpectedly retried Messages alias", paths)
			}
		})
	}
}

func TestMessagesAlternateRouteDoesNotRetryDecodeErrors(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `not-json`)
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+"/v1/chat/messages")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil)
	if err == nil {
		t.Fatal("complete() error = nil, want decode error")
	}
	if !slices.Equal(paths, []string{"/v1/chat/messages"}) {
		t.Errorf("request paths = %v, want no alternate route retry", paths)
	}
}

func TestMessagesRoutesPreserveConfiguredHostAndQuery(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantType EndpointType
		wantURL  string
		wantAlt  string
		wantList string
	}{
		{
			name:     "non-OpenCode Go-like path",
			endpoint: "https://example.test:8443/zen/go/v1/responses?tenant=a%2Fb&region=eu",
			wantType: EndpointResponses,
			wantURL:  "https://example.test:8443/zen/go/v1/chat/messages?tenant=a%2Fb&region=eu",
			wantAlt:  "https://example.test:8443/zen/go/v1/messages?tenant=a%2Fb&region=eu",
			wantList: "https://example.test:8443/zen/go/v1/models?tenant=a%2Fb&region=eu",
		},
		{
			name:     "OpenCode Go legacy path",
			endpoint: "https://opencode.ai/zen/go/v1/responses?tenant=a%2Fb&region=eu",
			wantType: EndpointResponses,
			wantURL:  "https://opencode.ai/zen/go/v1/chat/messages?tenant=a%2Fb&region=eu",
			wantAlt:  "https://opencode.ai/zen/go/v1/messages?tenant=a%2Fb&region=eu",
			wantList: "https://opencode.ai/zen/go/v1/models?tenant=a%2Fb&region=eu",
		},
		{
			name:     "OpenCode custom path",
			endpoint: "https://opencode.ai/zen/go/v1/custom/responses?tenant=a%2Fb&region=eu",
			wantType: EndpointResponses,
			wantURL:  "https://opencode.ai/zen/go/v1/custom/chat/messages?tenant=a%2Fb&region=eu",
			wantAlt:  "https://opencode.ai/zen/go/v1/custom/messages?tenant=a%2Fb&region=eu",
			wantList: "https://opencode.ai/zen/go/v1/custom/models?tenant=a%2Fb&region=eu",
		},
		{
			name:     "unrecognized custom URL",
			endpoint: "https://opencode.ai/custom/endpoint?tenant=a%2Fb&region=eu",
			wantType: EndpointChatCompletions,
			wantURL:  "https://opencode.ai/custom/endpoint?tenant=a%2Fb&region=eu",
			wantList: "https://opencode.ai/custom/endpoint/models?tenant=a%2Fb&region=eu",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			endpoints, configured, modelsURL := configureEndpoints(parsed, test.endpoint)
			if configured != test.wantType {
				t.Errorf("configured endpoint = %q, want %q", configured, test.wantType)
			}
			if modelsURL != test.wantList {
				t.Errorf("models URL = %q, want %q", modelsURL, test.wantList)
			}
			if test.wantAlt == "" {
				if len(endpoints) != 1 || endpoints[0].url != test.wantURL || endpoints[0].alternateURL != "" {
					t.Errorf("endpoints = %#v, want single unchanged custom endpoint", endpoints)
				}
				return
			}
			var messages endpointCandidate
			for _, endpoint := range endpoints {
				if endpoint.typeName == EndpointMessages {
					messages = endpoint
					break
				}
			}
			if messages.url != test.wantURL || messages.alternateURL != test.wantAlt {
				t.Errorf("Messages routes = %q / %q, want %q / %q", messages.url, messages.alternateURL, test.wantURL, test.wantAlt)
			}
		})
	}
}

func TestOpenCodeGoMessagesUseConfiguredRouteAndAuthenticationHeaders(t *testing.T) {
	transport := &headerCaptureTransport{onRequest: func(req *http.Request) (*http.Response, error) {
		if req.URL.RequestURI() != "/zen/go/v1/messages?tenant=eu" {
			t.Errorf("request URI = %q, want configured legacy Messages route", req.URL.RequestURI())
		}
		for header, want := range map[string]string{
			"Authorization":       "Bearer secret",
			"X-Api-Key":           "secret",
			"Anthropic-Version":   "2023-06-01",
			openCodeSessionHeader: "chat-42",
		} {
			if got := req.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		return jsonCaptureResponse(`{"role":"assistant","model":"model","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`), nil
	}}
	client, err := New("secret", "model", "https://opencode.ai/zen/go/v1/messages?tenant=eu", ClientOptions{
		SessionID:  "chat-42",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if _, err := client.complete(context.Background(), []Message{{Role: "user", Content: "question"}}, nil); err != nil {
		t.Fatalf("complete() error: %v", err)
	}
}
