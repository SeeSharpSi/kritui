package mcp

import (
	"errors"
	"net/http"
)

// newHTTPClient builds the HTTP client used for one server connection. When a
// token is configured, every outgoing request carries an Authorization bearer
// header through a cloning RoundTripper. Redirects are refused so the token
// can never be forwarded to another host.
func newHTTPClient(authorizationToken string) *http.Client {
	var baseTransport http.RoundTripper
	if authorizationToken == "" {
		baseTransport = http.DefaultTransport
	} else {
		baseTransport = &bearerTransport{token: authorizationToken}
	}
	return &http.Client{
		// Timeouts are deliberately unset: long-lived Streamable HTTP
		// sessions may hold SSE responses open, so request lifetimes are
		// bounded by contexts instead of http.Client.Timeout.
		Transport: baseTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("mcp: refusing to follow redirect")
		},
	}
}

// bearerTransport adds a bearer Authorization header to every request. The
// incoming request is cloned before mutation, and the token never appears in
// errors because transport failures are reported by the base transport.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	cloned := request.Clone(request.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(cloned)
}
