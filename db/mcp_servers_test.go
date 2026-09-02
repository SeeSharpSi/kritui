package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func openMCPServersTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return openMessagesTestDatabase(t, "")
}

func insertMCPServerRow(t *testing.T, database *sql.DB, id, name, url, token *string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO mcp_servers (id, position, name, url, authorization_token)
		VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM mcp_servers), ?, ?, ?)
	`, id, name, url, token); err != nil {
		t.Fatalf("insert mcp server row: %v", err)
	}
}

func storedMCPServerTokens(t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.Query(`SELECT id, authorization_token FROM mcp_servers`)
	if err != nil {
		t.Fatalf("query stored mcp server tokens: %v", err)
	}
	defer rows.Close()
	tokens := map[string]string{}
	for rows.Next() {
		var id string
		var token sql.NullString
		if err := rows.Scan(&id, &token); err != nil {
			t.Fatalf("scan stored mcp server token: %v", err)
		}
		if token.Valid {
			tokens[id] = token.String
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored mcp server tokens: %v", err)
	}
	return tokens
}

func storedDefaultToolNames(t *testing.T, database *sql.DB) []string {
	t.Helper()
	return storedToolNames(t, database, `SELECT name FROM default_tools ORDER BY position`)
}

func storedChatToolNames(t *testing.T, database *sql.DB, chatID int64) []string {
	t.Helper()
	return storedToolNames(t, database, `SELECT name FROM chat_tools WHERE chat_id = ? ORDER BY position`, chatID)
}

func storedToolNames(t *testing.T, database *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := database.Query(query, args...)
	if err != nil {
		t.Fatalf("query tool names: %v", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tool name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tool names: %v", err)
	}
	return names
}

func TestValidateMCPServerID(t *testing.T) {
	for _, id := range []string{"mcp-0123456789abcdef", "mcp-ffffffffffffffff"} {
		if err := ValidateMCPServerID(id); err != nil {
			t.Errorf("ValidateMCPServerID(%q) error: %v", id, err)
		}
	}
	for _, id := range []string{
		"",
		"mcp-",
		"mcp-0123456789abcde",
		"mcp-0123456789abcdef0",
		"MCP-0123456789abcdef",
		"mcp_0123456789abcdef",
		"mcp-0123456789ABCDEF",
		"mcp-0123456789abcdeg",
		"append-0123456789abcdef",
	} {
		if err := ValidateMCPServerID(id); err == nil {
			t.Errorf("ValidateMCPServerID(%q) error = nil, want error", id)
		}
	}
}

func TestMCPServerCapabilityRoundTrip(t *testing.T) {
	const id = "mcp-0123456789abcdef"
	capability := MCPServerCapability(id)
	if capability != "mcp_server_0123456789abcdef" {
		t.Errorf("MCPServerCapability(%q) = %q, want %q", id, capability, "mcp_server_0123456789abcdef")
	}
	if len(capability) > 64 {
		t.Errorf("capability %q exceeds the 64-byte tool name limit", capability)
	}
	for index := 0; index < len(capability); index++ {
		b := capability[index]
		if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-' {
			continue
		}
		t.Errorf("capability %q holds invalid tool name character at byte %d", capability, index)
	}
	parsed, ok := ParseMCPServerCapability(capability)
	if !ok || parsed != id {
		t.Errorf("ParseMCPServerCapability(%q) = (%q, %t), want (%q, true)", capability, parsed, ok, id)
	}
}

func TestParseMCPServerCapabilityRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{
		"",
		"webfetch",
		"mcp_server_",
		"mcp_server_0123456789abcde",
		"mcp_server_0123456789abcdef0",
		"mcp_server_0123456789ABCDEF",
		"mcp_server_0123456789abcdeg",
		"mcp_server_0123456789abcde f",
		"xmcp_server_0123456789abcdef",
		"mcp-server_0123456789abcdef",
	} {
		if id, ok := ParseMCPServerCapability(name); ok {
			t.Errorf("ParseMCPServerCapability(%q) = (%q, true), want false", name, id)
		}
	}
}

func TestValidateMCPServersAcceptsValidUpdates(t *testing.T) {
	updates := []MCPServerUpdate{
		{
			Server:              MCPServer{ID: "mcp-0123456789abcdef", Name: strings.Repeat("a", 80), URL: "https://docs.example.com/" + strings.Repeat("a", 2023)},
			AuthorizationChange: MCPReplaceAuthorization,
			AuthorizationValue:  "token-a",
		},
		{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "search", URL: "http://search.internal:8080/mcp"}},
		{Server: MCPServer{ID: "mcp-0000000000000000", Name: "legacy", URL: "https://legacy.example.com"}, AuthorizationChange: MCPClearAuthorization},
	}
	if err := ValidateMCPServers(updates); err != nil {
		t.Errorf("ValidateMCPServers() error: %v", err)
	}
}

func TestValidateMCPServersRejectsInvalidUpdates(t *testing.T) {
	valid := MCPServerUpdate{
		Server:              MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com/mcp"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "token-a",
	}
	tooMany := make([]MCPServerUpdate, 0, maxMCPServers+1)
	for index := 0; index <= maxMCPServers; index++ {
		tooMany = append(tooMany, MCPServerUpdate{
			Server: MCPServer{ID: fmt.Sprintf("mcp-%016x", index), Name: fmt.Sprintf("server %d", index), URL: "https://example.com"},
		})
	}
	tests := []struct {
		name    string
		updates []MCPServerUpdate
	}{
		{name: "too many servers", updates: tooMany},
		{name: "invalid id", updates: []MCPServerUpdate{{Server: MCPServer{ID: "append-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}}}},
		{name: "missing name", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "   ", URL: "https://docs.example.com"}}}},
		{name: "name too long", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: strings.Repeat("a", 81), URL: "https://docs.example.com"}}}},
		{
			name: "duplicate name case-insensitive",
			updates: []MCPServerUpdate{
				{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}},
				{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "DOCS", URL: "https://other.example.com"}},
			},
		},
		{name: "missing URL", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "  "}}}},
		{name: "relative URL", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "docs.example.com/mcp"}}}},
		{name: "unsupported scheme", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "ftp://docs.example.com"}}}},
		{name: "missing host", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https:///mcp"}}}},
		{name: "userinfo in URL", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://user:secret@docs.example.com/mcp"}}}},
		{name: "fragment in URL", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com/mcp#fragment"}}}},
		{name: "URL too long", updates: []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com/" + strings.Repeat("a", 2024)}}}},
		{name: "replace without value", updates: []MCPServerUpdate{{Server: valid.Server, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "   "}}},
		{name: "keep with value", updates: []MCPServerUpdate{{Server: valid.Server, AuthorizationChange: MCPKeepAuthorization, AuthorizationValue: "token-a"}}},
		{name: "clear with value", updates: []MCPServerUpdate{{Server: valid.Server, AuthorizationChange: MCPClearAuthorization, AuthorizationValue: "token-a"}}},
		{name: "unknown change mode", updates: []MCPServerUpdate{{Server: valid.Server, AuthorizationChange: MCPAuthorizationChange(9)}}},
		{
			name: "duplicate id",
			updates: []MCPServerUpdate{
				{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}},
				{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "other", URL: "https://other.example.com"}},
			},
		},
		{
			name: "oversized token",
			updates: []MCPServerUpdate{{
				Server:              valid.Server,
				AuthorizationChange: MCPReplaceAuthorization,
				AuthorizationValue:  strings.Repeat("a", maxMCPServerTokenBytes+1),
			}},
		},
	}
	for _, test := range tests {
		if err := ValidateMCPServers(test.updates); err == nil {
			t.Errorf("ValidateMCPServers(%s) error = nil, want error", test.name)
		}
	}
}

func TestGetMCPServersReturnsEmptyWhenUnconfigured(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	servers, err := GetMCPServers(context.Background(), database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("GetMCPServers() = %#v, want empty list", servers)
	}
}

func TestGetMCPServersExposesPresenceWithoutSecrets(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	const secret = "super-secret-token"
	storedSecret := secret
	insertMCPServerRow(t, database, strPtr("mcp-0123456789abcdef"), strPtr("docs"), strPtr("https://docs.example.com"), &storedSecret)
	insertMCPServerRow(t, database, strPtr("mcp-fedcba9876543210"), strPtr("blank"), strPtr("https://blank.example.com"), strPtr("   "))
	insertMCPServerRow(t, database, strPtr("mcp-0000000000000000"), strPtr("open"), strPtr("https://open.example.com"), nil)

	servers, err := GetMCPServers(context.Background(), database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	want := []MCPServer{
		{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com", AuthorizationConfigured: true},
		{ID: "mcp-fedcba9876543210", Name: "blank", URL: "https://blank.example.com", AuthorizationConfigured: false},
		{ID: "mcp-0000000000000000", Name: "open", URL: "https://open.example.com", AuthorizationConfigured: false},
	}
	if !slices.Equal(servers, want) {
		t.Fatalf("GetMCPServers() = %#v, want %#v", servers, want)
	}
	for _, server := range servers {
		for _, value := range []string{server.ID, server.Name, server.URL, fmt.Sprintf("%#v", server)} {
			if strings.Contains(value, secret) {
				t.Errorf("GetMCPServers leaked the secret in %q", server.ID)
			}
		}
	}
}

func TestSetMCPServersNormalizesSubmittedValues(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	update := MCPServerUpdate{
		Server:              MCPServer{ID: " mcp-0123456789abcdef ", Name: "  docs  ", URL: "  https://docs.example.com/mcp  "},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "  token-a  ",
	}
	if err := setMCPServers(context.Background(), database, []MCPServerUpdate{update}); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}
	servers, err := GetMCPServers(context.Background(), database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	want := []MCPServer{{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com/mcp", AuthorizationConfigured: true}}
	if !slices.Equal(servers, want) {
		t.Errorf("GetMCPServers() = %#v, want %#v", servers, want)
	}
}

func TestSetMCPServersStoresOrderedRows(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	updates := []MCPServerUpdate{
		{Server: MCPServer{ID: "mcp-0000000000000003", Name: "third", URL: "https://third.example.com"}},
		{Server: MCPServer{ID: "mcp-0000000000000001", Name: "first", URL: "https://first.example.com"}},
		{Server: MCPServer{ID: "mcp-0000000000000002", Name: "second", URL: "https://second.example.com"}},
	}
	if err := setMCPServers(context.Background(), database, updates); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}
	rows, err := database.Query(`SELECT id FROM mcp_servers ORDER BY position`)
	if err != nil {
		t.Fatalf("query mcp server positions: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan mcp server id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate mcp server positions: %v", err)
	}
	want := []string{"mcp-0000000000000003", "mcp-0000000000000001", "mcp-0000000000000002"}
	if !slices.Equal(ids, want) {
		t.Errorf("stored order = %v, want %v", ids, want)
	}
}

func TestSetMCPServersKeepReplaceClear(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := context.Background()

	first := []MCPServerUpdate{
		{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "token-a"},
		{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "search", URL: "https://search.example.com"}, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "token-b"},
	}
	if err := setMCPServers(ctx, database, first); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	second := []MCPServerUpdate{
		{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "search renamed", URL: "https://search.example.com/v2"}, AuthorizationChange: MCPKeepAuthorization},
		{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}, AuthorizationChange: MCPClearAuthorization},
		{Server: MCPServer{ID: "mcp-0000000000000000", Name: "new", URL: "https://new.example.com"}, AuthorizationChange: MCPKeepAuthorization},
	}
	if err := setMCPServers(ctx, database, second); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	tokens := storedMCPServerTokens(t, database)
	if tokens["mcp-fedcba9876543210"] != "token-b" {
		t.Errorf("Keep stored token = %q, want preserved token-b", tokens["mcp-fedcba9876543210"])
	}
	if _, ok := tokens["mcp-0123456789abcdef"]; ok {
		t.Errorf("Clear stored token = %q, want NULL", tokens["mcp-0123456789abcdef"])
	}
	if _, ok := tokens["mcp-0000000000000000"]; ok {
		t.Errorf("Keep on new server stored token = %q, want NULL", tokens["mcp-0000000000000000"])
	}

	servers, err := GetMCPServers(ctx, database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	want := []MCPServer{
		{ID: "mcp-fedcba9876543210", Name: "search renamed", URL: "https://search.example.com/v2", AuthorizationConfigured: true},
		{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com", AuthorizationConfigured: false},
		{ID: "mcp-0000000000000000", Name: "new", URL: "https://new.example.com", AuthorizationConfigured: false},
	}
	if !slices.Equal(servers, want) {
		t.Errorf("GetMCPServers() = %#v, want %#v", servers, want)
	}
}

func TestSetMCPServersRejectsInvalidUpdatesWithoutWriting(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := context.Background()
	valid := []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}}}
	if err := setMCPServers(ctx, database, valid); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}
	invalid := []MCPServerUpdate{{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "broken", URL: "not-a-url"}}}
	if err := setMCPServers(ctx, database, invalid); err == nil {
		t.Errorf("setMCPServers() error = nil, want error")
	}
	servers, err := GetMCPServers(ctx, database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != "mcp-0123456789abcdef" {
		t.Errorf("rejected write changed stored rows: %#v", servers)
	}
}

func TestSetMCPServersPrunesRemovedCapabilitySelections(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := context.Background()
	const keptID = "mcp-0123456789abcdef"
	const removedID = "mcp-fedcba9876543210"
	keptCapability := MCPServerCapability(keptID)
	removedCapability := MCPServerCapability(removedID)

	first := []MCPServerUpdate{
		{Server: MCPServer{ID: keptID, Name: "kept", URL: "https://kept.example.com"}},
		{Server: MCPServer{ID: removedID, Name: "removed", URL: "https://removed.example.com"}},
	}
	if err := setMCPServers(ctx, database, first); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert test chat: %v", err)
	}
	for position, name := range []string{"webfetch", keptCapability, removedCapability, "git"} {
		if _, err := database.Exec(`INSERT INTO default_tools (position, name) VALUES (?, ?)`, position, name); err != nil {
			t.Fatalf("insert default tool %q: %v", name, err)
		}
	}
	for position, name := range []string{keptCapability, removedCapability, "webfetch"} {
		if _, err := database.Exec(`INSERT INTO chat_tools (chat_id, position, name) VALUES (1, ?, ?)`, position, name); err != nil {
			t.Fatalf("insert chat tool %q: %v", name, err)
		}
	}

	second := []MCPServerUpdate{{Server: MCPServer{ID: keptID, Name: "kept", URL: "https://kept.example.com"}}}
	if err := setMCPServers(ctx, database, second); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	if got, want := storedDefaultToolNames(t, database), []string{"webfetch", keptCapability, "git"}; !slices.Equal(got, want) {
		t.Errorf("default tools after pruning = %v, want %v", got, want)
	}
	if got, want := storedChatToolNames(t, database, 1), []string{keptCapability, "webfetch"}; !slices.Equal(got, want) {
		t.Errorf("chat tools after pruning = %v, want %v", got, want)
	}
}

func TestGetMCPServerConfigsSelectsInSubmittedOrder(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := context.Background()
	updates := []MCPServerUpdate{
		{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "token-a"},
		{Server: MCPServer{ID: "mcp-fedcba9876543210", Name: "search", URL: "https://search.example.com"}, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "token-b"},
		{Server: MCPServer{ID: "mcp-0000000000000000", Name: "open", URL: "https://open.example.com"}},
	}
	if err := setMCPServers(ctx, database, updates); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	empty, err := GetMCPServerConfigs(ctx, database, nil)
	if err != nil {
		t.Fatalf("GetMCPServerConfigs() error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("GetMCPServerConfigs(nil) = %#v, want empty list", empty)
	}

	configs, err := GetMCPServerConfigs(ctx, database, []string{
		MCPServerCapability("mcp-0000000000000000"),
		MCPServerCapability("mcp-0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("GetMCPServerConfigs() error: %v", err)
	}
	want := []MCPServerConfig{
		{ID: "mcp-0000000000000000", Name: "open", URL: "https://open.example.com"},
		{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com", AuthorizationToken: "token-a"},
	}
	if !slices.Equal(configs, want) {
		t.Errorf("GetMCPServerConfigs() = %#v, want %#v", configs, want)
	}
}

func TestGetMCPServerConfigsRejectsUnknownAndDuplicateCapabilities(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	ctx := context.Background()
	updates := []MCPServerUpdate{{Server: MCPServer{ID: "mcp-0123456789abcdef", Name: "docs", URL: "https://docs.example.com"}, AuthorizationChange: MCPReplaceAuthorization, AuthorizationValue: "token-a"}}
	if err := setMCPServers(ctx, database, updates); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	if _, err := GetMCPServerConfigs(ctx, database, []string{MCPServerCapability("mcp-fedcba9876543210")}); !errors.Is(err, ErrMCPServerNotFound) {
		t.Errorf("GetMCPServerConfigs(unknown stored capability) error = %v, want ErrMCPServerNotFound", err)
	}
	if _, err := GetMCPServerConfigs(ctx, database, []string{"webfetch"}); !errors.Is(err, ErrMCPServerNotFound) {
		t.Errorf("GetMCPServerConfigs(malformed capability) error = %v, want ErrMCPServerNotFound", err)
	}
	docsCapability := MCPServerCapability("mcp-0123456789abcdef")
	if _, err := GetMCPServerConfigs(ctx, database, []string{docsCapability, docsCapability}); err == nil || errors.Is(err, ErrMCPServerNotFound) {
		t.Errorf("GetMCPServerConfigs(duplicate capability) error = %v, want non-sentinel rejection", err)
	}
}

func strPtr(value string) *string {
	return &value
}
