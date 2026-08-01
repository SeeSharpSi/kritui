package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestWebFetchRequestAndRetryResults(t *testing.T) {
	for _, test := range []struct {
		name, result, wantError string
		challenge               bool
	}{
		{name: "initial error", result: "error", wantError: "webfetch: send request:"},
		{name: "retry error", result: "error", wantError: "webfetch: send retry:", challenge: true},
		{name: "initial timeout", result: "timeout", wantError: "webfetch: request timed out"},
		{name: "retry timeout", result: "timeout", wantError: "webfetch: request timed out", challenge: true},
		{name: "retry success", result: "success", challenge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var userAgents []string
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				userAgents = append(userAgents, request.Header.Get("User-Agent"))
				if test.challenge && len(userAgents) == 1 {
					return challengeResponse(request), nil
				}
				switch test.result {
				case "error":
					return nil, errors.New("failed")
				case "timeout":
					<-request.Context().Done()
					return nil, request.Context().Err()
				default:
					return webResponse(request, http.StatusOK, "retried content"), nil
				}
			})}

			arguments := json.RawMessage(`{"url":"https://example.com","timeout":0.001}`)
			result, err := (WebFetchTool{HTTPClient: client}).Execute(t.Context(), arguments)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Execute() error = %v, want containing %q", err, test.wantError)
				}
			} else {
				var response webFetchResponse
				if err != nil || json.Unmarshal([]byte(result), &response) != nil || response.Content != "retried content" {
					t.Fatalf("Execute() = %q, %v, want retried content", result, err)
				}
			}
			wantCalls := 1
			if test.challenge {
				wantCalls = 2
			}
			if len(userAgents) != wantCalls || userAgents[0] != webFetchBrowserUserAgent || test.challenge && userAgents[1] != "opencode" {
				t.Errorf("User-Agent values = %q, want browser then optional opencode", userAgents)
			}
		})
	}
}

func challengeResponse(request *http.Request) *http.Response {
	response := webResponse(request, http.StatusForbidden, "challenge")
	response.Header.Set("cf-mitigated", "challenge")
	return response
}

func webResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
