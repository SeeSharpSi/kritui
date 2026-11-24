package kritui_db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestProviderCRUDRoundTripEncrypted(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	ids, err := SaveProviders(ctx, database, []ProviderDraft{
		{
			Name:         "Primary",
			Endpoint:     "https://primary.example/v1/responses",
			APIKeyChange: LLMReplaceAPIKey,
			APIKeyValue:  "primary-secret",
		},
		{
			Name:         "Secondary",
			Endpoint:     "https://secondary.example/v1/messages",
			APIKeyChange: LLMKeepAPIKey,
		},
	})
	if err != nil {
		t.Fatalf("SaveProviders() error: %v", err)
	}
	if len(ids) != 2 || ids[0] == 0 || ids[1] == 0 {
		t.Fatalf("SaveProviders() IDs = %v, want two nonzero IDs", ids)
	}

	settings, err := ListProviderSettings(ctx, database)
	if err != nil {
		t.Fatalf("ListProviderSettings() error: %v", err)
	}
	if len(settings) != 2 {
		t.Fatalf("provider settings = %#v, want two", settings)
	}
	if settings[0].Name != "Primary" || settings[0].Endpoint != "https://primary.example/v1/responses" || !settings[0].APIKeyConfigured || settings[0].Position != 0 {
		t.Errorf("primary settings = %#v", settings[0])
	}
	if settings[1].Name != "Secondary" || settings[1].APIKeyConfigured || settings[1].Position != 1 {
		t.Errorf("secondary settings = %#v", settings[1])
	}

	config, err := GetProviderConfig(ctx, database, ids[0])
	if err != nil {
		t.Fatalf("GetProviderConfig() error: %v", err)
	}
	if config.Endpoint != "https://primary.example/v1/responses" || config.APIKey != "primary-secret" {
		t.Errorf("provider config = %#v", config)
	}
	configs, err := ListProviderConfigs(ctx, database)
	if err != nil || len(configs) != 2 || configs[1].APIKey != "" {
		t.Fatalf("ListProviderConfigs() = %#v, %v", configs, err)
	}

	var endpoint, apiKey sql.NullString
	if err := database.QueryRow(`SELECT endpoint, api_key FROM providers WHERE id = ?`, ids[0]).Scan(&endpoint, &apiKey); err != nil {
		t.Fatalf("read raw provider: %v", err)
	}
	assertEncryptedWithoutMarkers(t, "providers.endpoint", endpoint, "primary.example", "primary-secret")
	assertEncryptedWithoutMarkers(t, "providers.api_key", apiKey, "primary-secret")
}

func TestSaveProvidersPreservesBlankKeyAndClears(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	cipher := newEncryptedStorageTestCipher(t)
	ctx := encryptedStorageTestContext(LegacyOwnerID, cipher)

	ids, err := SaveProviders(ctx, database, []ProviderDraft{{
		Name:         "Primary",
		Endpoint:     "https://primary.example/v1",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "secret",
	}})
	if err != nil {
		t.Fatalf("SaveProviders() error: %v", err)
	}
	id := ids[0]

	if _, err := SaveProviders(ctx, database, []ProviderDraft{{
		ID:           id,
		Name:         "Primary",
		Endpoint:     "https://primary.example/v2",
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  "   ",
	}}); err != nil {
		t.Fatalf("SaveProviders() blank key error: %v", err)
	}
	config, err := GetProviderConfig(ctx, database, id)
	if err != nil {
		t.Fatalf("GetProviderConfig() after blank key: %v", err)
	}
	if config.APIKey != "secret" || config.Endpoint != "https://primary.example/v2" {
		t.Fatalf("config after blank key = %#v, want preserved key and new endpoint", config)
	}

	if _, err := SaveProviders(ctx, database, []ProviderDraft{{
		ID:           id,
		Name:         "Primary",
		Endpoint:     "https://primary.example/v2",
		APIKeyChange: LLMClearAPIKey,
	}}); err != nil {
		t.Fatalf("SaveProviders() clear key error: %v", err)
	}
	config, err = GetProviderConfig(ctx, database, id)
	if err != nil {
		t.Fatalf("GetProviderConfig() after clear: %v", err)
	}
	if config.APIKey != "" {
		t.Errorf("API key after clear = %q, want empty", config.APIKey)
	}
	var apiKey sql.NullString
	if err := database.QueryRow(`SELECT api_key FROM providers WHERE id = ?`, id).Scan(&apiKey); err != nil {
		t.Fatal(err)
	}
	if apiKey.Valid {
		t.Errorf("raw API key after clear = %q, want NULL", apiKey.String)
	}
}

func TestSaveProvidersRejectsDuplicatesAndInvalidDrafts(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	for name, drafts := range map[string][]ProviderDraft{
		"duplicate names": {
			{Name: "Primary", Endpoint: "https://a.example"},
			{Name: " primary ", Endpoint: "https://b.example"},
		},
		"empty name": {
			{Name: " ", Endpoint: "https://a.example"},
		},
		"long name": {
			{Name: strings.Repeat("n", 101), Endpoint: "https://a.example"},
		},
		"missing endpoint": {
			{Name: "Primary"},
		},
		"invalid endpoint": {
			{Name: "Primary", Endpoint: "not-a-url"},
		},
		"environment ID": {
			{ID: EnvProviderID, Name: "Primary", Endpoint: "https://a.example"},
		},
		"duplicate IDs": {
			{ID: 99, Name: "Primary", Endpoint: "https://a.example"},
			{ID: 99, Name: "Secondary", Endpoint: "https://b.example"},
		},
	} {
		if _, err := SaveProviders(ctx, database, drafts); err == nil {
			t.Errorf("SaveProviders(%s) error = nil, want error", name)
		}
	}
}

func TestProviderOperationsAreAccountScoped(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	ids, err := SaveProviders(aliceCtx, database, []ProviderDraft{{
		Name: "Alice", Endpoint: "https://alice.example",
	}})
	if err != nil {
		t.Fatalf("SaveProviders(alice) error: %v", err)
	}
	aliceProvider := ids[0]

	if _, err := GetProviderConfig(bobCtx, database, aliceProvider); err == nil {
		t.Error("GetProviderConfig(bob) returned alice's provider")
	}
	if _, err := SaveProviders(bobCtx, database, []ProviderDraft{{
		ID: aliceProvider, Name: "Stolen", Endpoint: "https://stolen.example",
	}}); err == nil {
		t.Error("SaveProviders(bob) updated alice's provider")
	}
	if err := SetChatModelSelection(bobCtx, database, 1, aliceProvider, "model"); err == nil {
		t.Error("SetChatModelSelection(bob) accepted alice's provider")
	}
	if got, err := ListProviderSettings(bobCtx, database); err != nil || len(got) != 0 {
		t.Errorf("ListProviderSettings(bob) = %#v, %v, want empty", got, err)
	}
	_ = alice
}

func TestSaveProvidersDeletesOmittedProvidersAndPreferences(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	ids, err := SaveProviders(ctx, database, []ProviderDraft{
		{Name: "Keep", Endpoint: "https://keep.example"},
		{Name: "Drop", Endpoint: "https://drop.example"},
	})
	if err != nil {
		t.Fatalf("SaveProviders() error: %v", err)
	}
	keepID, dropID := ids[0], ids[1]
	if err := SetModelEndpointType(ctx, database, keepID, "keep-model", llm.EndpointMessages); err != nil {
		t.Fatal(err)
	}
	if err := SetModelEndpointType(ctx, database, dropID, "drop-model", llm.EndpointMessages); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO chats (id, user_id, model, provider_id) VALUES (1, 1, 'drop-model', ?)`, dropID); err != nil {
		t.Fatal(err)
	}

	if _, err := SaveProviders(ctx, database, []ProviderDraft{{
		ID: keepID, Name: "Keep", Endpoint: "https://keep.example",
	}}); err != nil {
		t.Fatalf("SaveProviders() remove error: %v", err)
	}

	if _, err := GetProviderConfig(ctx, database, dropID); err == nil {
		t.Error("omitted provider still readable")
	}
	var preferences int
	if err := database.QueryRow(`SELECT COUNT(*) FROM model_endpoint_preferences WHERE provider_id = ?`, dropID).Scan(&preferences); err != nil {
		t.Fatal(err)
	}
	if preferences != 0 {
		t.Errorf("omitted provider preferences = %d, want 0", preferences)
	}
	var chatProvider sql.NullInt64
	if err := database.QueryRow(`SELECT provider_id FROM chats WHERE id = 1`).Scan(&chatProvider); err != nil {
		t.Fatal(err)
	}
	if chatProvider.Valid {
		t.Errorf("chat provider after delete = %d, want NULL", chatProvider.Int64)
	}
	if _, err := GetProviderConfig(ctx, database, keepID); err != nil {
		t.Errorf("kept provider error: %v", err)
	}
}

func TestProviderColumnsRoundTrip(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()
	ids, err := SaveProviders(ctx, database, []ProviderDraft{{
		Name: "Primary", Endpoint: "https://primary.example",
	}})
	if err != nil {
		t.Fatalf("SaveProviders() error: %v", err)
	}
	providerID := ids[0]

	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:             "default-model",
		MaxToolRounds:     4,
		DefaultProviderID: &providerID,
		TitleProviderID:   &providerID,
		SummaryProviderID: &providerID,
		TitleGeneration:   &TitleGenerationSettingsUpdate{Enabled: true, Model: "title-model"},
	}); err != nil {
		t.Fatalf("SaveSettings() error: %v", err)
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

	if _, err := database.Exec(`INSERT INTO chats (id, user_id) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := SetChatModelSelection(ctx, database, 1, providerID, "chat-model"); err != nil {
		t.Fatalf("SetChatModelSelection() error: %v", err)
	}
	gotProvider, gotModel, err := GetChatModelSelection(ctx, database, 1)
	if err != nil || gotProvider != providerID || gotModel != "chat-model" {
		t.Errorf("chat selection = (%d, %q, %v), want (%d, chat-model)", gotProvider, gotModel, err, providerID)
	}

	if _, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "provider-input",
		Delivery:     "queue",
		Model:        "inbox-model",
		ProviderID:   providerID,
		Message:      llm.Message{Role: "user", Content: "hello"},
	}); err != nil {
		t.Fatalf("EnqueuePrompt() error: %v", err)
	}
	items, err := GetInbox(ctx, database, 1)
	if err != nil || len(items) != 1 || items[0].ProviderID != providerID {
		t.Fatalf("inbox = %#v, %v, want provider %d", items, err, providerID)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{
		Pending: true, Model: "completion-model", ProviderID: providerID,
	}); err != nil {
		t.Fatalf("SetCompletionState() error: %v", err)
	}
	state, err := GetCompletionState(ctx, database, 1)
	if err != nil || state.ProviderID != providerID {
		t.Errorf("completion state provider = %d, %v, want %d", state.ProviderID, err, providerID)
	}

	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:             "default-model",
		MaxToolRounds:     4,
		DefaultProviderID: int64Ptr(0),
		TitleProviderID:   int64Ptr(0),
		SummaryProviderID: int64Ptr(0),
	}); err != nil {
		t.Fatalf("SaveSettings() clear providers error: %v", err)
	}
	if got, err := GetDefaultProviderID(ctx, database); err != nil || got != 0 {
		t.Errorf("cleared default provider = %d, %v, want 0", got, err)
	}
	if err := SetChatModelSelection(ctx, database, 1, 0, ""); err != nil {
		t.Fatalf("SetChatModelSelection() clear error: %v", err)
	}
	if providerID, model, err := GetChatModelSelection(ctx, database, 1); err != nil || providerID != 0 || model != "" {
		t.Errorf("cleared chat selection = (%d, %q, %v)", providerID, model, err)
	}
}

func TestSaveSettingsRejectsForeignProvider(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	ids, err := SaveProviders(aliceCtx, database, []ProviderDraft{{
		Name: "Alice", Endpoint: "https://alice.example",
	}})
	if err != nil {
		t.Fatal(err)
	}
	providerID := ids[0]
	if err := SaveSettings(bobCtx, database, SettingsUpdate{
		Model:             "bob-model",
		MaxToolRounds:     4,
		DefaultProviderID: &providerID,
	}); err == nil {
		t.Error("SaveSettings(bob) accepted alice's provider")
	}
	if err := SaveSettings(aliceCtx, database, SettingsUpdate{
		Model:             "alice-model",
		MaxToolRounds:     4,
		DefaultProviderID: int64Ptr(EnvProviderID),
	}); err == nil {
		t.Error("SaveSettings() accepted the environment provider")
	}
	_ = alice
}

func int64Ptr(value int64) *int64 {
	return &value
}
