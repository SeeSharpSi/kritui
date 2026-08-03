package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
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

func TestWebFetchRejectsNonPublicDestinations(t *testing.T) {
	for _, address := range []string{
		"http://127.0.0.1",
		"http://10.0.0.1",
		"http://169.254.169.254",
		"http://192.168.1.1",
		"http://[::1]",
		"http://[fc00::1]",
		"http://[fe80::1]",
		"http://localhost",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := (WebFetchTool{}).Execute(t.Context(), json.RawMessage(`{"url":`+strconvQuote(address)+`}`))
			if err == nil || !strings.Contains(err.Error(), "public IP address") {
				t.Fatalf("Execute() error = %v, want public IP address rejection", err)
			}
		})
	}
}

func TestWebFetchIPPolicyRejectsMappedAndReservedAddresses(t *testing.T) {
	for _, address := range []string{
		"::ffff:127.0.0.1",
		"100.64.0.1",
		"192.0.2.1",
		"198.18.0.1",
		"2001:db8::1",
		"2002:7f00:1::",
		"4000::1",
		"fec0::1",
	} {
		if isPublicWebFetchIP(netip.MustParseAddr(address)) {
			t.Errorf("isPublicWebFetchIP(%q) = true, want false", address)
		}
	}
	if !isPublicWebFetchIP(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Error("isPublicWebFetchIP(public IPv6) = false, want true")
	}
}

func TestWebFetchRejectsCredentialsAndAlternatePorts(t *testing.T) {
	for _, test := range []struct {
		address, want string
	}{
		{address: "https://user:secret@example.com", want: "credentials are not allowed"},
		{address: "http://example.com:8080", want: "default port"},
		{address: "https://example.com:8443", want: "default port"},
	} {
		t.Run(test.address, func(t *testing.T) {
			_, err := (WebFetchTool{}).Execute(t.Context(), json.RawMessage(`{"url":`+strconvQuote(test.address)+`}`))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestWebFetchValidatesEveryRedirect(t *testing.T) {
	var requested []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.String())
		switch request.URL.Hostname() {
		case "example.com":
			return redirectResponse(request, "https://example.net/next"), nil
		case "example.net":
			return redirectResponse(request, "http://127.0.0.1/private"), nil
		default:
			return webResponse(request, http.StatusOK, "private"), nil
		}
	})}

	_, err := (WebFetchTool{HTTPClient: client}).Execute(t.Context(), json.RawMessage(`{"url":"https://example.com/start"}`))
	if err == nil || !strings.Contains(err.Error(), "public IP address") {
		t.Fatalf("Execute() error = %v, want redirect destination rejection", err)
	}
	if len(requested) != 2 {
		t.Fatalf("requested URLs = %q, want two public hops only", requested)
	}
}

func TestWebFetchAppliesURLPolicyToRedirects(t *testing.T) {
	for _, test := range []struct {
		name, location, want string
	}{
		{name: "credentials", location: "https://user:secret@example.net", want: "credentials are not allowed"},
		{name: "alternate port", location: "https://example.net:8443", want: "default port"},
		{name: "unsupported scheme", location: "ftp://example.net/file", want: "must start with http:// or https://"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				return redirectResponse(request, test.location), nil
			})}

			_, err := (WebFetchTool{HTTPClient: client}).Execute(t.Context(), json.RawMessage(`{"url":"https://example.com"}`))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute() error = %v, want containing %q", err, test.want)
			}
			if requests != 1 {
				t.Errorf("request count = %d, want redirect blocked before second request", requests)
			}
		})
	}
}

func TestWebFetchAllowsPublicDestination(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return webResponse(request, http.StatusOK, "public content"), nil
	})}

	result, err := (WebFetchTool{HTTPClient: client}).Execute(t.Context(), json.RawMessage(`{"url":"https://example.com"}`))
	if err != nil || !strings.Contains(result, "public content") {
		t.Fatalf("Execute() = %q, %v, want public content", result, err)
	}
}

func TestWebFetchDialerRejectsDNSResolvingToNonPublicAddress(t *testing.T) {
	dialCalled := false
	dialer := webFetchDialer{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}, nil
		},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	}

	_, err := dialer.DialContext(t.Context(), "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "non-public IP address") {
		t.Fatalf("DialContext() error = %v, want non-public address rejection", err)
	}
	if dialCalled {
		t.Fatal("DialContext() dialed before validating every resolved address")
	}
}

func TestWebFetchDialerDialsResolvedPublicAddress(t *testing.T) {
	wantErr := errors.New("dial stopped")
	var dialed string
	dialer := webFetchDialer{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			return nil, wantErr
		},
	}

	_, err := dialer.DialContext(t.Context(), "tcp", "example.com:443")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DialContext() error = %v, want %v", err, wantErr)
	}
	if dialed != "93.184.216.34:443" {
		t.Errorf("dialed address = %q, want resolved public IP", dialed)
	}
}

func challengeResponse(request *http.Request) *http.Response {
	response := webResponse(request, http.StatusForbidden, "challenge")
	response.Header.Set("cf-mitigated", "challenge")
	return response
}

func redirectResponse(request *http.Request, location string) *http.Response {
	response := webResponse(request, http.StatusFound, "redirect")
	response.Header.Set("Location", location)
	return response
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
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
