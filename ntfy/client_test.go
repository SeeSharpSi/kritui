package ntfy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublishSendsTopicNotification(t *testing.T) {
	var method, path, authorization, contentType, title, message string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		title = r.Header.Get("Title")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		message = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	err := (Client{HTTPClient: server.Client()}).Publish(context.Background(), Config{
		Endpoint: server.URL + "/base/",
		Topic:    "response-ready",
		APIKey:   "secret-token",
	}, "Kritui response ready", "Chat 7 has a response.")
	if err != nil {
		t.Fatalf("Publish() error: %v", err)
	}

	if method != http.MethodPost || path != "/base/response-ready" {
		t.Errorf("request = %s %q, want POST /base/response-ready", method, path)
	}
	if authorization != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want bearer token", authorization)
	}
	if contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", contentType)
	}
	if title != "Kritui response ready" {
		t.Errorf("Title = %q, want notification title", title)
	}
	if message != "Chat 7 has a response." {
		t.Errorf("message = %q, want metadata message", message)
	}
}

func TestPublishOmitsAuthorizationWithoutAPIKey(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := (Client{HTTPClient: server.Client()}).Publish(context.Background(), Config{
		Endpoint: server.URL,
		Topic:    "public-topic",
	}, "", "ready"); err != nil {
		t.Fatalf("Publish() error: %v", err)
	}
	if authorization != "" {
		t.Errorf("Authorization = %q, want omitted", authorization)
	}
}

func TestPublishRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{name: "missing endpoint", config: Config{Topic: "topic"}},
		{name: "missing topic", config: Config{Endpoint: "https://ntfy.example"}},
		{name: "unsupported scheme", config: Config{Endpoint: "file:///tmp/ntfy", Topic: "topic"}},
		{name: "missing host", config: Config{Endpoint: "https:///topic", Topic: "topic"}},
		{name: "endpoint credentials", config: Config{Endpoint: "https://user:pass@ntfy.example", Topic: "topic"}},
		{name: "endpoint query", config: Config{Endpoint: "https://ntfy.example?secret=value", Topic: "topic"}},
		{name: "invalid topic", config: Config{Endpoint: "https://ntfy.example", Topic: "bad topic"}},
		{name: "long topic", config: Config{Endpoint: "https://ntfy.example", Topic: strings.Repeat("a", 65)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := (Client{}).Publish(context.Background(), test.config, "", "message"); err == nil {
				t.Fatal("Publish() error = nil, want validation error")
			}
		})
	}
}

func TestPublishDoesNotExposeResponseBodyOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "remote secret detail", http.StatusForbidden)
	}))
	defer server.Close()

	err := (Client{HTTPClient: server.Client()}).Publish(context.Background(), Config{
		Endpoint: server.URL,
		Topic:    "topic",
	}, "", "message")
	if err == nil {
		t.Fatal("Publish() error = nil, want HTTP error")
	}
	if strings.Contains(err.Error(), "remote secret detail") {
		t.Errorf("Publish() error exposed response body: %v", err)
	}
}
