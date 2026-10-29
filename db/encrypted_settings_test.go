package kritui_db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const encryptedStorageTestPrefix = "enc:v1:"

func newEncryptedStorageTestCipher(t *testing.T) *StorageCipher {
	t.Helper()
	cipher, err := NewStorageCipher(bytes.Repeat([]byte{0x5a}, StorageKeySize))
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	return cipher
}

func encryptedStorageTestContext(userID int64, cipher *StorageCipher) context.Context {
	return WithStorageCipher(WithUser(context.Background(), userID), cipher)
}

func rawSettingsColumns(t *testing.T, database *sql.DB, userID int64) map[string]sql.NullString {
	t.Helper()
	var llmEndpoint, llmAPIKey, ntfyEndpoint, ntfyTopic, ntfyAPIKey sql.NullString
	if err := database.QueryRow(`
		SELECT llm_endpoint, llm_api_key, ntfy_endpoint, ntfy_topic, ntfy_api_key
		FROM settings
		WHERE id = ?
	`, userID).Scan(&llmEndpoint, &llmAPIKey, &ntfyEndpoint, &ntfyTopic, &ntfyAPIKey); err != nil {
		t.Fatalf("read raw settings: %v", err)
	}
	return map[string]sql.NullString{
		"settings.llm_endpoint":  llmEndpoint,
		"settings.llm_api_key":   llmAPIKey,
		"settings.ntfy_endpoint": ntfyEndpoint,
		"settings.ntfy_topic":    ntfyTopic,
		"settings.ntfy_api_key":  ntfyAPIKey,
	}
}

func assertEncryptedWithoutMarkers(t *testing.T, column string, value sql.NullString, markers ...string) string {
	t.Helper()
	if !value.Valid {
		t.Fatalf("%s is NULL, want encrypted text", column)
	}
	if !strings.HasPrefix(value.String, encryptedStorageTestPrefix) {
		t.Fatalf("%s = %q, want %s prefix", column, value.String, encryptedStorageTestPrefix)
	}
	for _, marker := range markers {
		if strings.Contains(value.String, marker) {
			t.Errorf("%s leaks marker %q in stored ciphertext", column, marker)
		}
	}
	return value.String
}

func TestEncryptedSettingsRoundTripAndRawCiphertext(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	encryptedCtx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	const (
		llmEndpoint  = "https://llm.example/v1?api_key=llm-endpoint-marker"
		llmAPIKey    = "llm-key-marker"
		ntfyEndpoint = "https://ntfy.example/ntfy-endpoint-marker"
		ntfyTopic    = "ntfy-topic-marker"
		ntfyAPIKey   = "ntfy-key-marker"
	)
	if err := SaveLLMSettings(encryptedCtx, database, LLMSettingsUpdate{
		Endpoint:     llmEndpoint,
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  llmAPIKey,
	}); err != nil {
		t.Fatalf("SaveLLMSettings() error: %v", err)
	}
	if err := SaveNtfySettings(encryptedCtx, database, NtfySettingsUpdate{
		Endpoint:     ntfyEndpoint,
		Topic:        ntfyTopic,
		APIKeyChange: NtfyReplaceAPIKey,
		APIKeyValue:  ntfyAPIKey,
	}); err != nil {
		t.Fatalf("SaveNtfySettings() error: %v", err)
	}

	raw := rawSettingsColumns(t, database, LegacyOwnerID)
	assertEncryptedWithoutMarkers(t, "settings.llm_endpoint", raw["settings.llm_endpoint"], "llm-endpoint-marker")
	assertEncryptedWithoutMarkers(t, "settings.llm_api_key", raw["settings.llm_api_key"], "llm-key-marker")
	assertEncryptedWithoutMarkers(t, "settings.ntfy_endpoint", raw["settings.ntfy_endpoint"], "ntfy-endpoint-marker")
	assertEncryptedWithoutMarkers(t, "settings.ntfy_topic", raw["settings.ntfy_topic"], "ntfy-topic-marker")
	assertEncryptedWithoutMarkers(t, "settings.ntfy_api_key", raw["settings.ntfy_api_key"], "ntfy-key-marker")

	llmPublic, err := GetLLMSettings(encryptedCtx, database)
	if err != nil {
		t.Fatalf("GetLLMSettings() error: %v", err)
	}
	if llmPublic.Endpoint != llmEndpoint || !llmPublic.APIKeyConfigured {
		t.Errorf("GetLLMSettings() = %#v, want endpoint and configured key", llmPublic)
	}
	llmPrivate, err := GetLLMConfig(encryptedCtx, database)
	if err != nil {
		t.Fatalf("GetLLMConfig() error: %v", err)
	}
	if llmPrivate.Endpoint != llmEndpoint || llmPrivate.APIKey != llmAPIKey {
		t.Errorf("GetLLMConfig() = %#v, want decrypted endpoint and key", llmPrivate)
	}

	ntfyPublic, err := GetNtfySettings(encryptedCtx, database)
	if err != nil {
		t.Fatalf("GetNtfySettings() error: %v", err)
	}
	if ntfyPublic.Endpoint != ntfyEndpoint || ntfyPublic.Topic != ntfyTopic || !ntfyPublic.APIKeyConfigured {
		t.Errorf("GetNtfySettings() = %#v, want decrypted destination and configured key", ntfyPublic)
	}
	ntfyPrivate, err := GetNtfyPublishConfig(encryptedCtx, database)
	if err != nil {
		t.Fatalf("GetNtfyPublishConfig() error: %v", err)
	}
	if ntfyPrivate.Endpoint != ntfyEndpoint || ntfyPrivate.Topic != ntfyTopic || ntfyPrivate.APIKey != ntfyAPIKey {
		t.Errorf("GetNtfyPublishConfig() = %#v, want decrypted config", ntfyPrivate)
	}

	// The legacy plaintext context still sees the same values only when it
	// carries no cipher, which existing direct-caller tests rely on.
	if got, err := GetLLMConfig(ctx, database); err != nil {
		t.Fatalf("GetLLMConfig() without cipher error: %v", err)
	} else if !strings.HasPrefix(got.APIKey, encryptedStorageTestPrefix) {
		t.Errorf("GetLLMConfig() without cipher = %#v, want raw ciphertext compatibility mode", got)
	}
}

func TestEncryptedSettingsKeepReplaceClear(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     "https://llm.example/v1/responses",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "first-llm-key",
	}); err != nil {
		t.Fatalf("seed SaveLLMSettings() error: %v", err)
	}
	firstKeyCiphertext := rawSettingsColumns(t, database, LegacyOwnerID)["settings.llm_api_key"].String

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint: "https://llm.example/v1/chat/completions",
	}); err != nil {
		t.Fatalf("keep SaveLLMSettings() error: %v", err)
	}
	config, err := GetLLMConfig(ctx, database)
	if err != nil {
		t.Fatalf("GetLLMConfig() after keep error: %v", err)
	}
	if config.Endpoint != "https://llm.example/v1/chat/completions" || config.APIKey != "first-llm-key" {
		t.Errorf("config after keep = %#v, want new endpoint and preserved key", config)
	}
	if got := rawSettingsColumns(t, database, LegacyOwnerID)["settings.llm_api_key"].String; got != firstKeyCiphertext {
		t.Error("keep changed stored llm_api_key ciphertext, want untouched secret")
	}

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     config.Endpoint,
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "second-llm-key",
	}); err != nil {
		t.Fatalf("replace SaveLLMSettings() error: %v", err)
	}
	config, err = GetLLMConfig(ctx, database)
	if err != nil {
		t.Fatalf("GetLLMConfig() after replace error: %v", err)
	}
	if config.APIKey != "second-llm-key" {
		t.Errorf("API key after replace = %q, want second-llm-key", config.APIKey)
	}
	if got := rawSettingsColumns(t, database, LegacyOwnerID)["settings.llm_api_key"].String; got == firstKeyCiphertext {
		t.Error("replace reused stored llm_api_key ciphertext, want new envelope")
	}

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     config.Endpoint,
		APIKeyChange: LLMClearAPIKey,
	}); err != nil {
		t.Fatalf("clear SaveLLMSettings() error: %v", err)
	}
	public, err := GetLLMSettings(ctx, database)
	if err != nil {
		t.Fatalf("GetLLMSettings() after clear error: %v", err)
	}
	if public.APIKeyConfigured {
		t.Error("API key configured after clear, want unset")
	}
	if cleared := rawSettingsColumns(t, database, LegacyOwnerID)["settings.llm_api_key"]; cleared.Valid {
		t.Error("llm_api_key after clear is not NULL")
	}

	if err := SaveNtfySettings(ctx, database, NtfySettingsUpdate{
		Endpoint:     "https://ntfy.example",
		Topic:        "topic",
		APIKeyChange: NtfyReplaceAPIKey,
		APIKeyValue:  "first-ntfy-key",
	}); err != nil {
		t.Fatalf("seed SaveNtfySettings() error: %v", err)
	}
	if err := SaveNtfySettings(ctx, database, NtfySettingsUpdate{
		Endpoint: "https://ntfy.example/new",
		Topic:    "new-topic",
	}); err != nil {
		t.Fatalf("keep SaveNtfySettings() error: %v", err)
	}
	ntfy, err := GetNtfyPublishConfig(ctx, database)
	if err != nil {
		t.Fatalf("GetNtfyPublishConfig() after keep error: %v", err)
	}
	if ntfy.Endpoint != "https://ntfy.example/new" || ntfy.Topic != "new-topic" || ntfy.APIKey != "first-ntfy-key" {
		t.Errorf("ntfy after keep = %#v, want new destination and preserved key", ntfy)
	}
	if err := SaveNtfySettings(ctx, database, NtfySettingsUpdate{
		Endpoint:     ntfy.Endpoint,
		Topic:        ntfy.Topic,
		APIKeyChange: NtfyClearAPIKey,
	}); err != nil {
		t.Fatalf("clear SaveNtfySettings() error: %v", err)
	}
	if got := rawSettingsColumns(t, database, LegacyOwnerID)["settings.ntfy_api_key"]; got.Valid {
		t.Error("ntfy_api_key after clear is not NULL")
	}
}

func TestEncryptedMCPServersRoundTripAndKeepReplaceClear(t *testing.T) {
	database := openMCPServersTestDatabase(t)
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)
	const serverID = "mcp-0123456789abcdef"

	first := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "docs-name-marker", URL: "https://docs.example/mcp?token=url-marker"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "token-marker",
	}}
	if err := setMCPServers(ctx, database, first); err != nil {
		t.Fatalf("setMCPServers() error: %v", err)
	}

	var rawName, rawURL, rawToken sql.NullString
	if err := database.QueryRow(`SELECT name, url, authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&rawName, &rawURL, &rawToken); err != nil {
		t.Fatalf("read raw mcp server: %v", err)
	}
	firstTokenCiphertext := assertEncryptedWithoutMarkers(t, "mcp_servers.authorization_token", rawToken, "token-marker")
	assertEncryptedWithoutMarkers(t, "mcp_servers.name", rawName, "docs-name-marker")
	assertEncryptedWithoutMarkers(t, "mcp_servers.url", rawURL, "url-marker")

	servers, err := GetMCPServers(ctx, database)
	if err != nil {
		t.Fatalf("GetMCPServers() error: %v", err)
	}
	want := []MCPServer{{ID: serverID, Name: "docs-name-marker", URL: "https://docs.example/mcp?token=url-marker", AuthorizationConfigured: true}}
	if len(servers) != 1 || servers[0] != want[0] {
		t.Errorf("GetMCPServers() = %#v, want %#v", servers, want)
	}
	configs, err := GetMCPServerConfigs(ctx, database, []string{MCPServerCapability(serverID)})
	if err != nil {
		t.Fatalf("GetMCPServerConfigs() error: %v", err)
	}
	if len(configs) != 1 || configs[0].AuthorizationToken != "token-marker" || configs[0].Name != "docs-name-marker" || configs[0].URL != "https://docs.example/mcp?token=url-marker" {
		t.Errorf("GetMCPServerConfigs() = %#v, want decrypted config", configs)
	}

	second := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "renamed", URL: "https://docs.example/v2"},
		AuthorizationChange: MCPKeepAuthorization,
	}}
	if err := setMCPServers(ctx, database, second); err != nil {
		t.Fatalf("keep setMCPServers() error: %v", err)
	}
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&rawToken); err != nil {
		t.Fatalf("read kept raw mcp token: %v", err)
	}
	if rawToken.String == firstTokenCiphertext {
		t.Error("Keep reused token ciphertext, want re-encrypted envelope")
	}
	assertEncryptedWithoutMarkers(t, "mcp_servers.authorization_token", rawToken, "token-marker")
	configs, err = GetMCPServerConfigs(ctx, database, []string{MCPServerCapability(serverID)})
	if err != nil {
		t.Fatalf("GetMCPServerConfigs() after keep error: %v", err)
	}
	if len(configs) != 1 || configs[0].AuthorizationToken != "token-marker" || configs[0].Name != "renamed" || configs[0].URL != "https://docs.example/v2" {
		t.Errorf("config after keep = %#v, want preserved token and new metadata", configs)
	}

	third := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "renamed", URL: "https://docs.example/v2"},
		AuthorizationChange: MCPClearAuthorization,
	}}
	if err := setMCPServers(ctx, database, third); err != nil {
		t.Fatalf("clear setMCPServers() error: %v", err)
	}
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&rawToken); err != nil {
		t.Fatalf("read cleared raw mcp token: %v", err)
	}
	if rawToken.Valid {
		t.Error("mcp authorization_token after clear is not NULL")
	}

	fourth := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "renamed", URL: "https://docs.example/v2"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "replacement-token",
	}}
	if err := setMCPServers(ctx, database, fourth); err != nil {
		t.Fatalf("replace setMCPServers() error: %v", err)
	}
	configs, err = GetMCPServerConfigs(ctx, database, []string{MCPServerCapability(serverID)})
	if err != nil {
		t.Fatalf("GetMCPServerConfigs() after replace error: %v", err)
	}
	if len(configs) != 1 || configs[0].AuthorizationToken != "replacement-token" {
		t.Errorf("config after replace = %#v, want replacement token", configs)
	}
}

func TestEncryptedPromptAppendsRoundTripAndRawCiphertext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	values := []PromptAppend{{
		ID:               "custom",
		Name:             "append-name-marker",
		Text:             "append-text-marker",
		EnabledByDefault: true,
	}}
	if err := setPromptAppends(ctx, database, values); err != nil {
		t.Fatalf("setPromptAppends() error: %v", err)
	}

	var id string
	var rawName, rawText sql.NullString
	var enabled bool
	if err := database.QueryRow(`SELECT id, name, text, enabled_by_default FROM prompt_appends WHERE id = ?`, "custom").Scan(&id, &rawName, &rawText, &enabled); err != nil {
		t.Fatalf("read raw prompt append: %v", err)
	}
	if id != "custom" || !enabled {
		t.Errorf("prompt append plaintext metadata = (%q, %t), want (custom, true)", id, enabled)
	}
	assertEncryptedWithoutMarkers(t, "prompt_appends.name", rawName, "append-name-marker")
	assertEncryptedWithoutMarkers(t, "prompt_appends.text", rawText, "append-text-marker")

	got, err := GetPromptAppends(ctx, database)
	if err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	}
	if len(got) != 1 || got[0] != values[0] {
		t.Errorf("GetPromptAppends() = %#v, want %#v", got, values)
	}

	// Built-in definitions remain embedded plaintext and need no cipher.
	defaults := DefaultPromptAppends()
	if len(defaults) == 0 || defaults[0].Name == "" || defaults[0].Text == "" {
		t.Error("DefaultPromptAppends() returned empty built-in definitions")
	}
}

func TestEncryptedMCPServerBlankNonNullTokenFailsAuthentication(t *testing.T) {
	const serverID = "mcp-0123456789abcdef"
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace", raw: "   "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openMCPServersTestDatabase(t)
			cipher := newEncryptedStorageTestCipher(t)
			ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

			seed := []MCPServerUpdate{{
				Server:              MCPServer{ID: serverID, Name: "docs", URL: "https://docs.example"},
				AuthorizationChange: MCPReplaceAuthorization,
				AuthorizationValue:  "valid-token",
			}}
			if err := setMCPServers(ctx, database, seed); err != nil {
				t.Fatalf("seed setMCPServers() error: %v", err)
			}
			if _, err := database.Exec(`UPDATE mcp_servers SET authorization_token = ? WHERE id = ?`, test.raw, serverID); err != nil {
				t.Fatalf("store raw non-NULL token: %v", err)
			}

			if _, err := GetMCPServerConfigs(ctx, database, []string{MCPServerCapability(serverID)}); !errors.Is(err, ErrStorageDecryption) {
				t.Errorf("GetMCPServerConfigs() non-NULL blank token error = %v, want ErrStorageDecryption", err)
			}

			keep := []MCPServerUpdate{{
				Server:              MCPServer{ID: serverID, Name: "docs", URL: "https://docs.example"},
				AuthorizationChange: MCPKeepAuthorization,
			}}
			if err := setMCPServers(ctx, database, keep); !errors.Is(err, ErrStorageDecryption) {
				t.Errorf("setMCPServers() keep non-NULL blank token error = %v, want ErrStorageDecryption", err)
			}

			var stored sql.NullString
			if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&stored); err != nil {
				t.Fatalf("read stored token after rejected keep: %v", err)
			}
			if !stored.Valid || stored.String != test.raw {
				t.Errorf("stored token after rejected keep = %#v, want unchanged raw %q", stored, test.raw)
			}
		})
	}
}

func TestEncryptedStorageRejectsTamperingAndForeignOwner(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)
	const apiKey = "tamper-target-marker"

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     "https://llm.example/v1/responses",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  apiKey,
	}); err != nil {
		t.Fatalf("SaveLLMSettings() error: %v", err)
	}
	ciphertext := rawSettingsColumns(t, database, LegacyOwnerID)["settings.llm_api_key"].String

	envelope, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, encryptedStorageTestPrefix))
	if err != nil {
		t.Fatalf("decode stored envelope: %v", err)
	}
	envelope[len(envelope)-1] ^= 0xff
	tampered := encryptedStorageTestPrefix + base64.StdEncoding.EncodeToString(envelope)
	if _, err := database.Exec(`UPDATE settings SET llm_api_key = ? WHERE id = ?`, tampered, LegacyOwnerID); err != nil {
		t.Fatalf("tamper stored llm_api_key: %v", err)
	}

	_, err = GetLLMConfig(ctx, database)
	if !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("GetLLMConfig() tampered error = %v, want ErrStorageDecryption", err)
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Errorf("tampered error leaked plaintext: %v", err)
	}
	if public, publicErr := GetLLMSettings(ctx, database); publicErr != nil {
		t.Fatalf("GetLLMSettings() error: %v", publicErr)
	} else if public.APIKeyConfigured != true {
		t.Error("tampered secret presence is hidden, want configured state from ciphertext presence")
	}

	foreign, err := CreateUser(context.Background(), database, "Foreign", "encoded-foreign")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	foreignCtx := encryptedStorageTestContext(foreign.ID, cipher)
	if _, err := DecryptStorageText(foreignCtx, "settings.llm_api_key", storageRowID(LegacyOwnerID), ciphertext); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("foreign-owner decrypt error = %v, want ErrStorageDecryption", err)
	}

	const serverID = "mcp-0123456789abcdef"
	ownerCtx := encryptedStorageTestContext(LegacyOwnerID, cipher)
	ownerServers := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "owner", URL: "https://owner.example"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "owner-token",
	}}
	if err := setMCPServers(ownerCtx, database, ownerServers); err != nil {
		t.Fatalf("owner setMCPServers() error: %v", err)
	}
	foreignServers := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "foreign", URL: "https://foreign.example"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "foreign-token",
	}}
	if err := setMCPServers(foreignCtx, database, foreignServers); err != nil {
		t.Fatalf("foreign setMCPServers() error: %v", err)
	}

	ownerConfigs, err := GetMCPServerConfigs(ownerCtx, database, []string{MCPServerCapability(serverID)})
	if err != nil {
		t.Fatalf("owner GetMCPServerConfigs() error: %v", err)
	}
	if len(ownerConfigs) != 1 || ownerConfigs[0].AuthorizationToken != "owner-token" || ownerConfigs[0].Name != "owner" {
		t.Errorf("owner config = %#v, want owner token", ownerConfigs)
	}
	foreignConfigs, err := GetMCPServerConfigs(foreignCtx, database, []string{MCPServerCapability(serverID)})
	if err != nil {
		t.Fatalf("foreign GetMCPServerConfigs() error: %v", err)
	}
	if len(foreignConfigs) != 1 || foreignConfigs[0].AuthorizationToken != "foreign-token" || foreignConfigs[0].Name != "foreign" {
		t.Errorf("foreign config = %#v, want foreign token", foreignConfigs)
	}
}
