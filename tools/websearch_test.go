package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchEmptyResultsReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"missing","results":[]}`))
	}))
	defer server.Close()

	result, err := NewWebSearchTool(server.URL).Execute(t.Context(), json.RawMessage(`{"query":"missing"}`))
	if err == nil || !strings.Contains(err.Error(), "SearXNG returned no results") {
		t.Fatalf("Execute() error = %v, want empty-results error", err)
	}
	if result != "" {
		t.Fatalf("Execute() result = %q, want empty result", result)
	}
}
