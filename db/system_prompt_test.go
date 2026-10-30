package kritui_db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func saveSystemPromptForTest(t *testing.T, ctx context.Context, database *sql.DB, value string) {
	t.Helper()
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "test-model",
		MaxToolRounds: 4,
		SystemPrompt:  &value,
	}); err != nil {
		t.Fatalf("SaveSettings(system prompt) error: %v", err)
	}
}

func TestGetSystemPromptFallsBackToDefault(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	prompt, err := GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != llm.DefaultSystemPrompt() {
		t.Errorf("GetSystemPrompt() = %q, want default %q", prompt, llm.DefaultSystemPrompt())
	}
	var stored sql.NullString
	if err := database.QueryRow(`SELECT system_prompt FROM settings WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("read raw system prompt: %v", err)
	}
	if stored.Valid {
		t.Errorf("raw system prompt = %q, want NULL", stored.String)
	}
}

func TestSaveSystemPromptPreservesExactCustomText(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	custom := "\n  Custom account instructions.  \n\nSecond paragraph.\n"
	saveSystemPromptForTest(t, ctx, database, custom)

	prompt, err := GetSystemPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Errorf("GetSystemPrompt() = %q, want exact custom %q", prompt, custom)
	}
}

func TestSaveSystemPromptBlankOrDefaultStoresNull(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveSystemPromptForTest(t, ctx, database, "Custom first.")

	for _, value := range []string{"", " \t\n ", "  " + llm.DefaultSystemPrompt() + "\n"} {
		saveSystemPromptForTest(t, ctx, database, value)
		var stored sql.NullString
		if err := database.QueryRow(`SELECT system_prompt FROM settings WHERE id = 1`).Scan(&stored); err != nil {
			t.Fatalf("read raw system prompt after %q: %v", value, err)
		}
		if stored.Valid {
			t.Errorf("raw system prompt after %q = %q, want NULL", value, stored.String)
		}
		prompt, err := GetSystemPrompt(ctx, database)
		if err != nil {
			t.Fatalf("GetSystemPrompt() after %q error: %v", value, err)
		}
		if prompt != llm.DefaultSystemPrompt() {
			t.Errorf("GetSystemPrompt() after %q = %q, want default", value, prompt)
		}
	}
}

func TestSaveSettingsOmittedSystemPromptIsPreserved(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveSystemPromptForTest(t, ctx, database, "Preserved custom prompt.")

	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "other-model",
		MaxToolRounds: 7,
	}); err != nil {
		t.Fatalf("SaveSettings(without system prompt) error: %v", err)
	}
	prompt, err := GetSystemPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != "Preserved custom prompt." {
		t.Errorf("GetSystemPrompt() = %q, want preserved custom prompt", prompt)
	}
}

func TestSystemPromptIsAccountScoped(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	other, err := CreateUser(ctx, database, "PromptOwner", "prompt-owner-hash")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	otherCtx := WithUser(ctx, other.ID)

	saveSystemPromptForTest(t, ctx, database, "Legacy account prompt.")
	saveSystemPromptForTest(t, otherCtx, database, "Other account prompt.")

	legacyPrompt, err := GetSystemPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetSystemPrompt(legacy) error: %v", err)
	}
	if legacyPrompt != "Legacy account prompt." {
		t.Errorf("legacy prompt = %q, want legacy custom prompt", legacyPrompt)
	}
	otherPrompt, err := GetSystemPrompt(otherCtx, database)
	if err != nil {
		t.Fatalf("GetSystemPrompt(other) error: %v", err)
	}
	if otherPrompt != "Other account prompt." {
		t.Errorf("other prompt = %q, want other account custom prompt", otherPrompt)
	}
}

func TestValidateSystemPromptSizeLimit(t *testing.T) {
	if err := ValidateSystemPrompt(strings.Repeat("a", MaxSystemPromptBytes)); err != nil {
		t.Errorf("ValidateSystemPrompt(max) error = %v, want nil", err)
	}
	if err := ValidateSystemPrompt(strings.Repeat("a", MaxSystemPromptBytes+1)); err == nil {
		t.Error("ValidateSystemPrompt(over limit) error = nil, want error")
	}
}

func TestSaveSettingsSystemPromptFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveSystemPromptForTest(t, ctx, database, "Original prompt.")

	if _, err := database.Exec(`
		CREATE TRIGGER fail_system_prompt_update
		BEFORE UPDATE OF system_prompt ON settings
		BEGIN
			SELECT RAISE(ABORT, 'injected system prompt failure');
		END;
	`); err != nil {
		t.Fatalf("inject failure trigger: %v", err)
	}

	changed := "Changed prompt."
	err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "changed-model",
		MaxToolRounds: 9,
		SystemPrompt:  &changed,
	})
	if err == nil {
		t.Fatal("SaveSettings() error = nil, want injected system prompt failure")
	}
	if model, getErr := GetDefaultModel(ctx, database, "fallback"); getErr != nil {
		t.Fatalf("GetDefaultModel() error: %v", getErr)
	} else if model != "test-model" {
		t.Errorf("default model after rollback = %q, want test-model", model)
	}
	prompt, err := GetSystemPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != "Original prompt." {
		t.Errorf("system prompt after rollback = %q, want original", prompt)
	}
}
