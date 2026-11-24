package kritui_db

import (
	"context"
	"database/sql"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestMigrateLegacyProviderConfigMovesReferences(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     "https://legacy.example/v1/responses",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "legacy-secret",
	}); err != nil {
		t.Fatalf("SaveLLMSettings() error: %v", err)
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "default-model",
		MaxToolRounds: 4,
		ToolResultElision: &llm.ToolResultElisionConfig{
			Mode:         llm.ToolResultElisionSummarize,
			SummaryModel: "summary-model",
		},
		TitleGeneration: &TitleGenerationSettingsUpdate{Enabled: true, Model: "title-model"},
	}); err != nil {
		t.Fatalf("SaveSettings() error: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO chats (id, user_id, model) VALUES (1, 1, 'chat-model')`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "legacy-input",
		Delivery:     "queue",
		Model:        "inbox-model",
		Message:      llm.Message{Role: "user", Content: "legacy"},
	}); err != nil {
		t.Fatalf("EnqueuePrompt() error: %v", err)
	}
	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "completion-model"}); err != nil {
		t.Fatalf("SetCompletionState() error: %v", err)
	}
	if err := SetModelEndpointType(ctx, database, 0, "chat-model", llm.EndpointMessages); err != nil {
		t.Fatalf("SetModelEndpointType() error: %v", err)
	}

	if err := MigrateLegacyProviderConfig(ctx, database); err != nil {
		t.Fatalf("MigrateLegacyProviderConfig() error: %v", err)
	}

	providers, err := ListProviderSettings(ctx, database)
	if err != nil {
		t.Fatalf("ListProviderSettings() error: %v", err)
	}
	if len(providers) != 1 || providers[0].Name != "Default" || providers[0].Endpoint != "https://legacy.example/v1/responses" || !providers[0].APIKeyConfigured {
		t.Fatalf("providers after migration = %#v", providers)
	}
	providerID := providers[0].ID
	config, err := GetProviderConfig(ctx, database, providerID)
	if err != nil || config.APIKey != "legacy-secret" {
		t.Fatalf("migrated provider config = %#v, %v", config, err)
	}

	if got, err := GetDefaultProviderID(ctx, database); err != nil || got != providerID {
		t.Errorf("default provider = %d, %v, want %d", got, err, providerID)
	}
	if got, err := GetToolResultElisionSummaryProviderID(ctx, database); err != nil || got != providerID {
		t.Errorf("summary provider = %d, %v, want %d", got, err, providerID)
	}
	title, err := GetTitleGenerationSettings(ctx, database)
	if err != nil || title.ProviderID != providerID {
		t.Errorf("title provider = %d, %v, want %d", title.ProviderID, err, providerID)
	}

	gotProvider, gotModel, err := GetChatModelSelection(ctx, database, 1)
	if err != nil || gotProvider != providerID || gotModel != "chat-model" {
		t.Errorf("chat selection = (%d, %q, %v), want (%d, chat-model)", gotProvider, gotModel, err, providerID)
	}
	items, err := GetInbox(ctx, database, 1)
	if err != nil || len(items) != 1 || items[0].ProviderID != providerID {
		t.Errorf("inbox provider = %#v, %v, want %d", items, err, providerID)
	}
	state, err := GetCompletionState(ctx, database, 1)
	if err != nil || state.ProviderID != providerID {
		t.Errorf("completion provider = %d, %v, want %d", state.ProviderID, err, providerID)
	}
	if got, err := GetModelEndpointType(ctx, database, providerID, "chat-model"); err != nil || got != llm.EndpointMessages {
		t.Errorf("migrated endpoint preference = %q, %v", got, err)
	}
	if got, err := GetModelEndpointType(ctx, database, 0, "chat-model"); err != nil || got != "" {
		t.Errorf("legacy endpoint preference still present = %q, %v", got, err)
	}

	var endpoint, apiKey sql.NullString
	if err := database.QueryRow(`SELECT llm_endpoint, llm_api_key FROM settings WHERE id = 1`).Scan(&endpoint, &apiKey); err != nil {
		t.Fatal(err)
	}
	if endpoint.Valid || apiKey.Valid {
		t.Errorf("legacy settings not cleared: endpoint=%v api_key=%v", endpoint, apiKey)
	}

	if err := MigrateLegacyProviderConfig(ctx, database); err != nil {
		t.Fatalf("second MigrateLegacyProviderConfig() error: %v", err)
	}
	providers, err = ListProviderSettings(ctx, database)
	if err != nil || len(providers) != 1 {
		t.Fatalf("providers after idempotent migration = %#v, %v", providers, err)
	}
}

func TestMigrateLegacyProviderConfigSkipsAccountsWithProviders(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	ids, err := SaveProviders(ctx, database, []ProviderDraft{{
		Name: "Existing", Endpoint: "https://existing.example",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     "https://legacy.example/v1",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "legacy-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyProviderConfig(ctx, database); err != nil {
		t.Fatalf("MigrateLegacyProviderConfig() error: %v", err)
	}

	providers, err := ListProviderSettings(ctx, database)
	if err != nil || len(providers) != 1 || providers[0].ID != ids[0] || providers[0].Name != "Existing" {
		t.Fatalf("providers after skipped migration = %#v, %v", providers, err)
	}
	var endpoint sql.NullString
	if err := database.QueryRow(`SELECT llm_endpoint FROM settings WHERE id = 1`).Scan(&endpoint); err != nil {
		t.Fatal(err)
	}
	if !endpoint.Valid {
		t.Error("legacy endpoint cleared for account that already had providers")
	}
}

func TestMigrateLegacyProviderConfigIgnoresEnvironmentProvider(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	if err := SetModelEndpointType(ctx, database, EnvProviderID, "env-model", llm.EndpointMessages); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyProviderConfig(ctx, database); err != nil {
		t.Fatalf("MigrateLegacyProviderConfig() error: %v", err)
	}
	if got, err := GetModelEndpointType(ctx, database, EnvProviderID, "env-model"); err != nil || got != llm.EndpointMessages {
		t.Errorf("environment preference = %q, %v, want preserved", got, err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM providers WHERE id = ?`, EnvProviderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("environment provider was stored")
	}
}
