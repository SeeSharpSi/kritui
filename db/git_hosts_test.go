package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func gitHostTestContext() context.Context {
	return WithUser(context.Background(), LegacyOwnerID)
}

func TestDefaultGitHosts(t *testing.T) {
	values := DefaultGitHosts()
	if len(values) != 4 {
		t.Fatalf("DefaultGitHosts() length = %d, want 4", len(values))
	}
	for _, value := range values {
		if err := ValidateGitHostID(value.ID); err != nil {
			t.Errorf("default host ID %q: %v", value.ID, err)
		}
		if value.TokenConfigured {
			t.Errorf("default host %q unexpectedly has a token", value.Host)
		}
	}
}

func TestGetGitHostsReturnsDefaultsWhenUnconfigured(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	values, err := GetGitHosts(gitHostTestContext(), database)
	if err != nil {
		t.Fatalf("GetGitHosts() error: %v", err)
	}
	if !reflect.DeepEqual(values, DefaultGitHosts()) {
		t.Fatalf("GetGitHosts() = %#v, want defaults", values)
	}
	configs, err := GetGitHostConfigs(gitHostTestContext(), database)
	if err != nil {
		t.Fatalf("GetGitHostConfigs() error: %v", err)
	}
	if len(configs) != 4 || configs[0].Token != "" {
		t.Fatalf("GetGitHostConfigs() = %#v, want public defaults", configs)
	}
}

func TestSetGitHostsRoundTripAndSecrets(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := WithStorageCipher(gitHostTestContext(), newEncryptedStorageTestCipher(t))
	updates := []GitHostUpdate{
		{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com", Username: "octocat"}, TokenChange: GitReplaceToken, TokenValue: "ghp-secret"},
		{Host: GitHost{ID: "git-fedcba9876543210", Host: "git.example.com:8443"}},
	}
	if err := setGitHosts(ctx, database, updates); err != nil {
		t.Fatalf("setGitHosts() error: %v", err)
	}
	values, err := GetGitHosts(ctx, database)
	if err != nil {
		t.Fatalf("GetGitHosts() error: %v", err)
	}
	want := []GitHost{
		{ID: "git-0123456789abcdef", Host: "github.com", Username: "octocat", TokenConfigured: true},
		{ID: "git-fedcba9876543210", Host: "git.example.com:8443"},
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("GetGitHosts() = %#v, want %#v", values, want)
	}
	configs, err := GetGitHostConfigs(ctx, database)
	if err != nil {
		t.Fatalf("GetGitHostConfigs() error: %v", err)
	}
	wantConfigs := []GitHostConfig{
		{Host: "github.com", Username: "octocat", Token: "ghp-secret"},
		{Host: "git.example.com:8443"},
	}
	if !reflect.DeepEqual(configs, wantConfigs) {
		t.Fatalf("GetGitHostConfigs() = %#v, want %#v", configs, wantConfigs)
	}

	var storedHost, storedToken sql.NullString
	if err := database.QueryRow(`SELECT host, token FROM git_hosts WHERE id = 'git-0123456789abcdef'`).Scan(&storedHost, &storedToken); err != nil {
		t.Fatalf("read stored host: %v", err)
	}
	if !storedHost.Valid || storedHost.String == "github.com" {
		t.Fatalf("stored host = %q, want encrypted", storedHost.String)
	}
	if !storedToken.Valid || storedToken.String == "ghp-secret" || strings.Contains(storedToken.String, "ghp-secret") {
		t.Fatalf("stored token = %q, want encrypted", storedToken.String)
	}
}

func TestSetGitHostsKeepReplaceClear(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := gitHostTestContext()
	base := []GitHostUpdate{{
		Host: GitHost{ID: "git-0123456789abcdef", Host: "git.example.com"}, TokenChange: GitReplaceToken, TokenValue: "first-token",
	}}
	if err := setGitHosts(ctx, database, base); err != nil {
		t.Fatalf("seed setGitHosts() error: %v", err)
	}

	keep := []GitHostUpdate{{
		Host: GitHost{ID: "git-0123456789abcdef", Host: "renamed.example.com"},
	}}
	if err := setGitHosts(ctx, database, keep); err != nil {
		t.Fatalf("keep setGitHosts() error: %v", err)
	}
	configs, err := GetGitHostConfigs(ctx, database)
	if err != nil {
		t.Fatalf("get kept config: %v", err)
	}
	if len(configs) != 1 || configs[0].Host != "renamed.example.com" || configs[0].Token != "first-token" {
		t.Fatalf("kept config = %#v, want renamed host with preserved token", configs)
	}

	replace := []GitHostUpdate{{
		Host: GitHost{ID: "git-0123456789abcdef", Host: "git.example.com"}, TokenChange: GitReplaceToken, TokenValue: "second-token",
	}}
	if err := setGitHosts(ctx, database, replace); err != nil {
		t.Fatalf("replace setGitHosts() error: %v", err)
	}
	configs, err = GetGitHostConfigs(ctx, database)
	if err != nil || len(configs) != 1 || configs[0].Token != "second-token" {
		t.Fatalf("replaced config = %#v, %v", configs, err)
	}

	clear := []GitHostUpdate{{
		Host: GitHost{ID: "git-0123456789abcdef", Host: "git.example.com"}, TokenChange: GitClearToken,
	}}
	if err := setGitHosts(ctx, database, clear); err != nil {
		t.Fatalf("clear setGitHosts() error: %v", err)
	}
	values, err := GetGitHosts(ctx, database)
	if err != nil || len(values) != 1 || values[0].TokenConfigured {
		t.Fatalf("cleared values = %#v, %v, want token cleared", values, err)
	}
}

func TestSetGitHostsEmptyClearsAllowlistWhenConfigured(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := gitHostTestContext()
	if err := setGitHosts(ctx, database, []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com"}}}); err != nil {
		t.Fatalf("seed setGitHosts() error: %v", err)
	}
	if err := setGitHosts(ctx, database, []GitHostUpdate{}); err != nil {
		t.Fatalf("clear setGitHosts() error: %v", err)
	}
	values, err := GetGitHosts(ctx, database)
	if err != nil {
		t.Fatalf("GetGitHosts() error: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("GetGitHosts() = %#v, want empty configured allowlist", values)
	}
}

func TestValidateGitHostsRejectsInvalidUpdates(t *testing.T) {
	valid := GitHostUpdate{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com"}}
	tooMany := make([]GitHostUpdate, maxGitHosts+1)
	for index := range tooMany {
		tooMany[index] = GitHostUpdate{Host: GitHost{ID: "git-0123456789abcdef", Host: "example.com"}}
	}
	tests := []struct {
		name    string
		updates []GitHostUpdate
	}{
		{name: "too many", updates: tooMany},
		{name: "invalid id", updates: []GitHostUpdate{{Host: GitHost{ID: "bad", Host: "github.com"}}}},
		{name: "missing host", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef"}}}},
		{name: "scheme", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "https://github.com"}}}},
		{name: "path", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com/org"}}}},
		{name: "userinfo", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "user@github.com"}}}},
		{name: "wildcard", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "*.example.com"}}}},
		{name: "bad port", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com:70000"}}}},
		{name: "bad username", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com", Username: "bad user"}}}},
		{name: "colon username", updates: []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com", Username: "bad:user"}}}},
		{name: "duplicate id", updates: []GitHostUpdate{valid, {Host: GitHost{ID: valid.Host.ID, Host: "gitlab.com"}}}},
		{name: "duplicate host", updates: []GitHostUpdate{valid, {Host: GitHost{ID: "git-fedcba9876543210", Host: "GITHUB.COM"}}}},
		{name: "keep with value", updates: []GitHostUpdate{{Host: valid.Host, TokenChange: GitKeepToken, TokenValue: "token"}}},
		{name: "clear with value", updates: []GitHostUpdate{{Host: valid.Host, TokenChange: GitClearToken, TokenValue: "token"}}},
		{name: "replace empty", updates: []GitHostUpdate{{Host: valid.Host, TokenChange: GitReplaceToken}}},
		{name: "unknown mode", updates: []GitHostUpdate{{Host: valid.Host, TokenChange: GitTokenChange(9)}}},
	}
	for _, test := range tests {
		if err := ValidateGitHosts(test.updates); err == nil {
			t.Errorf("ValidateGitHosts(%s) error = nil, want error", test.name)
		}
	}
}

func TestNormalizeGitHost(t *testing.T) {
	tests := map[string]string{
		"github.com":            "github.com",
		" GitHub.COM ":          "github.com",
		"github.com:443":        "github.com",
		"github.com:8443":       "github.com:8443",
		"git.example.com.":      "git.example.com",
		"[::1]:8443":            "[::1]:8443",
		"git.example.com:00001": "git.example.com:1",
	}
	for input, want := range tests {
		got, err := normalizeGitHost(input)
		if err != nil || got != want {
			t.Errorf("normalizeGitHost(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
}

func TestSetGitHostsRejectsInvalidWithoutWriting(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := gitHostTestContext()
	if err := setGitHosts(ctx, database, []GitHostUpdate{{Host: GitHost{ID: "git-0123456789abcdef", Host: "github.com"}}}); err != nil {
		t.Fatalf("seed setGitHosts() error: %v", err)
	}
	err := setGitHosts(ctx, database, []GitHostUpdate{{Host: GitHost{ID: "git-fedcba9876543210", Host: "gitlab.com"}}, {Host: GitHost{ID: "bad"}}})
	if err == nil {
		t.Fatal("setGitHosts() error = nil, want validation failure")
	}
	values, err := GetGitHosts(ctx, database)
	if err != nil {
		t.Fatalf("GetGitHosts() error: %v", err)
	}
	if len(values) != 1 || values[0].Host != "github.com" {
		t.Fatalf("GetGitHosts() = %#v, want original allowlist", values)
	}
}

func TestGitHostsAreScopedPerUser(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ownerCtx := gitHostTestContext()
	foreignCtx := WithUser(context.Background(), 2)
	if _, err := database.Exec(`INSERT INTO users (id, username, password_hash) VALUES (2, 'alice', 'hash');
		INSERT INTO settings (id) VALUES (2);`); err != nil {
		t.Fatalf("seed second user: %v", err)
	}
	ownerUpdate := GitHostUpdate{
		Host:        GitHost{ID: "git-0123456789abcdef", Host: "github.com"},
		TokenChange: GitReplaceToken,
		TokenValue:  "owner-token",
	}
	if err := setGitHosts(ownerCtx, database, []GitHostUpdate{ownerUpdate}); err != nil {
		t.Fatalf("owner setGitHosts() error: %v", err)
	}
	foreign, err := GetGitHosts(foreignCtx, database)
	if err != nil {
		t.Fatalf("foreign GetGitHosts() error: %v", err)
	}
	if !reflect.DeepEqual(foreign, DefaultGitHosts()) {
		t.Fatalf("foreign GetGitHosts() = %#v, want defaults", foreign)
	}
	foreignConfigs, err := GetGitHostConfigs(foreignCtx, database)
	if err != nil {
		t.Fatalf("foreign GetGitHostConfigs() error: %v", err)
	}
	for _, config := range foreignConfigs {
		if config.Token != "" {
			t.Fatalf("foreign config leaked token: %#v", config)
		}
	}
}

func TestGetGitHostConfigsRejectsTamperedCiphertext(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := WithStorageCipher(gitHostTestContext(), newTestStorageCipher(t))
	update := GitHostUpdate{
		Host:        GitHost{ID: "git-0123456789abcdef", Host: "github.com"},
		TokenChange: GitReplaceToken,
		TokenValue:  "secret",
	}
	if err := setGitHosts(ctx, database, []GitHostUpdate{update}); err != nil {
		t.Fatalf("setGitHosts() error: %v", err)
	}
	if _, err := database.Exec(`UPDATE git_hosts SET token = 'enc:v1:tampered'`); err != nil {
		t.Fatalf("tamper token: %v", err)
	}
	if _, err := GetGitHostConfigs(ctx, database); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("GetGitHostConfigs() error = %v, want ErrStorageDecryption", err)
	}
}
