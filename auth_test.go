package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
)

func TestSafeNextPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: "/"},
		{name: "root", raw: "/", want: "/"},
		{name: "chat deep link", raw: "/?chat=12", want: "/?chat=12"},
		{name: "root query", raw: "/?chat=12&tab=history", want: "/?chat=12&tab=history"},
		{name: "encoded query", raw: "/?q=a%20b", want: "/?q=a%20b"},
		{name: "fragment", raw: "/?chat=1#messages", want: "/"},
		{name: "absolute URL", raw: "https://evil.example/", want: "/"},
		{name: "scheme relative", raw: "//evil.example/", want: "/"},
		{name: "backslash", raw: `/\\evil.example/`, want: "/"},
		{name: "encoded backslash path", raw: "/%5Cevil", want: "/"},
		{name: "encoded double slash path", raw: "/%2F%2Fevil", want: "/"},
		{name: "control character", raw: "/\x00evil", want: "/"},
		{name: "non-root path", raw: "/settings", want: "/"},
		{name: "parent traversal", raw: "/../secret", want: "/"},
		{name: "query only", raw: "?chat=1", want: "/"},
		{name: "leading space", raw: " /", want: "/"},
		{name: "oversized", raw: "/?" + strings.Repeat("a", authNextMaxBytes), want: "/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeNextPath(test.raw); got != test.want {
				t.Fatalf("safeNextPath(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestSameOriginRootPath(t *testing.T) {
	request := httptest.NewRequest("GET", "http://kritui.example/?chat=9", nil)
	request.Host = "kritui.example"

	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "same origin", raw: "https://kritui.example/?chat=9", want: "/?chat=9", ok: true},
		{name: "case-insensitive host", raw: "http://KRITUI.EXAMPLE/?chat=9", want: "/?chat=9", ok: true},
		{name: "non-root current path", raw: "http://kritui.example/settings", want: "/", ok: true},
		{name: "different host", raw: "http://evil.example/?chat=9", want: "", ok: false},
		{name: "different port", raw: "http://kritui.example:8443/?chat=9", want: "", ok: false},
		{name: "relative", raw: "/?chat=9", want: "", ok: false},
		{name: "javascript scheme", raw: "javascript:alert(1)", want: "", ok: false},
		{name: "empty", raw: "", want: "", ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := sameOriginRootPath(request, test.raw)
			if got != test.want || ok != test.ok {
				t.Fatalf("sameOriginRootPath(%q) = (%q, %v), want (%q, %v)", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestSessionTokenValidation(t *testing.T) {
	token, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43", len(token))
	}
	if !validSessionToken(token) {
		t.Fatal("validSessionToken rejected a generated token")
	}
	if verifier := hashSessionToken(token); verifier == token || len(verifier) != 43 {
		t.Fatalf("hashSessionToken = %q, want a distinct 43-byte verifier", verifier)
	}
	if hashSessionToken(token) != hashSessionToken(token) {
		t.Fatal("hashSessionToken is not deterministic")
	}

	short := newTestTokenString(t, 31)
	long := newTestTokenString(t, 33)
	for name, candidate := range map[string]string{
		"empty":     "",
		"short":     short,
		"long":      long,
		"padding":   token[:42] + "=",
		"invalid":   token[:42] + "!",
		"oversized": strings.Repeat("a", authNextMaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if validSessionToken(candidate) {
				t.Fatalf("validSessionToken accepted %q", candidate)
			}
		})
	}
}

func newTestTokenString(t *testing.T, size int) string {
	t.Helper()
	encoded := make([]byte, size)
	for index := range encoded {
		encoded[index] = byte(index)
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func TestClientIPUsesRemoteAddressOnly(t *testing.T) {
	tests := map[string]string{
		"192.0.2.1:80":         "192.0.2.1",
		"[2001:db8::1]:443":    "2001:db8::1",
		"192.0.2.2":            "192.0.2.2",
		"":                     "unknown",
		"not-an-address:12345": "not-an-address:12345",
		"203.0.113.9:9999":     "203.0.113.9",
	}
	for input, want := range tests {
		if got := clientIP(input); got != want {
			t.Fatalf("clientIP(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAuthRateLimiterFixedWindow(t *testing.T) {
	limiter := newAuthRateLimiter(2, time.Minute)
	now := time.Unix(1_700_000_000, 0)

	if allowed, _ := limiter.allow("192.0.2.10", now); !allowed {
		t.Fatal("first attempt was rejected")
	}
	if allowed, _ := limiter.allow("192.0.2.10", now.Add(time.Second)); !allowed {
		t.Fatal("second attempt was rejected")
	}
	allowed, retry := limiter.allow("192.0.2.10", now.Add(2*time.Second))
	if allowed {
		t.Fatal("third attempt was allowed")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry duration = %v, want within the window", retry)
	}
	if allowed, _ := limiter.allow("192.0.2.11", now.Add(2*time.Second)); !allowed {
		t.Fatal("a different address shared the bucket")
	}
	if allowed, _ := limiter.allow("192.0.2.10", now.Add(time.Minute)); !allowed {
		t.Fatal("attempt was still rejected after the window reset")
	}
}

func TestAuthRateLimiterBoundsMemory(t *testing.T) {
	limiter := newAuthRateLimiter(10, time.Minute)
	limiter.maxEntries = 3
	now := time.Unix(1_700_000_000, 0)

	for _, key := range []string{"a", "b", "c", "d", "e"} {
		limiter.allow(key, now)
		if len(limiter.entries) > limiter.maxEntries {
			t.Fatalf("entries grew to %d with cap %d", len(limiter.entries), limiter.maxEntries)
		}
	}
	if len(limiter.entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(limiter.entries))
	}
	if _, ok := limiter.entries["e"]; !ok {
		t.Fatal("newest bucket was evicted")
	}

	limiter.maxEntries = 1
	limiter.entries = map[string]authRateLimitEntry{
		"stale": {count: 1, reset: now.Add(-time.Second)},
	}
	if allowed, _ := limiter.allow("fresh", now); !allowed {
		t.Fatal("fresh attempt was rejected")
	}
	if _, ok := limiter.entries["stale"]; ok {
		t.Fatal("expired bucket was not purged")
	}
	if len(limiter.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(limiter.entries))
	}
}

func TestSecureCookiesFromEnvironment(t *testing.T) {
	t.Setenv("KRITUI_SECURE_COOKIES", "")
	value, err := secureCookiesFromEnvironment()
	if err != nil || !value {
		t.Fatalf("empty setting = (%v, %v), want (true, nil)", value, err)
	}

	t.Setenv("KRITUI_SECURE_COOKIES", "true")
	value, err = secureCookiesFromEnvironment()
	if err != nil || !value {
		t.Fatalf("true setting = (%v, %v), want (true, nil)", value, err)
	}

	t.Setenv("KRITUI_SECURE_COOKIES", "false")
	value, err = secureCookiesFromEnvironment()
	if err != nil || value {
		t.Fatalf("false setting = (%v, %v), want (false, nil)", value, err)
	}

	for _, invalid := range []string{"TRUE", "1", "yes", " false"} {
		t.Setenv("KRITUI_SECURE_COOKIES", invalid)
		if _, err := secureCookiesFromEnvironment(); err == nil {
			t.Fatalf("secureCookiesFromEnvironment accepted %q", invalid)
		}
	}
}

func TestFreshDatabaseSeedsUnclaimedSkrittle(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()

	var username, passwordHash string
	var disabled int
	if err := database.QueryRow(`
		SELECT username, password_hash, disabled FROM users WHERE id = ?
	`, kritui_db.LegacyOwnerID).Scan(&username, &passwordHash, &disabled); err != nil {
		t.Fatalf("load seeded legacy account: %v", err)
	}
	if username != kritui_db.LegacyOwnerUsername || passwordHash != "" || disabled != 1 {
		t.Fatalf("seeded legacy account = %q hash %q disabled %d, want unclaimed Skrittle",
			username, passwordHash, disabled)
	}
	if _, err := kritui_db.GetUserByUsername(ctx, database, "skrittle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unclaimed Skrittle login lookup error = %v, want sql.ErrNoRows", err)
	}
	var settingsRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings WHERE id = ?`, kritui_db.LegacyOwnerID).Scan(&settingsRows); err != nil {
		t.Fatalf("count legacy settings: %v", err)
	}
	if settingsRows != 1 {
		t.Fatalf("legacy settings rows = %d, want 1", settingsRows)
	}
}

func TestCreateUserClaimsLegacyAccountOnce(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()

	if _, err := database.Exec(`
		UPDATE settings SET default_model = 'legacy-model', llm_api_key = 'legacy-llm'
		WHERE id = ?
	`, kritui_db.LegacyOwnerID); err != nil {
		t.Fatalf("seed legacy settings: %v", err)
	}

	owner, err := kritui_db.CreateUser(ctx, database, "skrittle", "first-hash")
	if err != nil {
		t.Fatalf("claim legacy account: %v", err)
	}
	if owner.ID != kritui_db.LegacyOwnerID || owner.Username != kritui_db.LegacyOwnerUsername ||
		owner.PasswordHash != "first-hash" {
		t.Fatalf("claimed owner = %#v", owner)
	}
	ownerCtx := kritui_db.WithUser(ctx, kritui_db.LegacyOwnerID)
	if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
		t.Fatalf("claimed owner model = %q, %v, want preserved legacy-model", got, err)
	}
	if config, err := kritui_db.GetLLMConfig(ownerCtx, database); err != nil || config.APIKey != "legacy-llm" {
		t.Fatalf("claimed owner LLM key = %q, %v, want preserved secret", config.APIKey, err)
	}

	for _, username := range []string{"Skrittle", "skrittle", "SKRITTLE"} {
		if _, err := kritui_db.CreateUser(ctx, database, username, "replacement-hash"); !errors.Is(err, kritui_db.ErrUsernameTaken) {
			t.Fatalf("CreateUser(%q) error = %v, want ErrUsernameTaken", username, err)
		}
	}
	stored, err := kritui_db.GetUserByUsername(ctx, database, "SKRITTLE")
	if err != nil {
		t.Fatalf("get claimed owner: %v", err)
	}
	if stored.PasswordHash != "first-hash" {
		t.Fatalf("claimed owner password hash = %q, want unchanged first-hash", stored.PasswordHash)
	}
}
