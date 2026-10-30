package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func saveCompactionPromptForTest(t *testing.T, ctx context.Context, database *sql.DB, value string) {
	t.Helper()
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:            "test-model",
		MaxToolRounds:    4,
		CompactionPrompt: &value,
	}); err != nil {
		t.Fatalf("SaveSettings(compaction prompt) error: %v", err)
	}
}

func TestGetCompactionPromptFallsBackToDefault(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	prompt, err := GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != llm.DefaultCompactionPrompt() {
		t.Errorf("GetCompactionPrompt() = %q, want default %q", prompt, llm.DefaultCompactionPrompt())
	}
	var stored sql.NullString
	if err := database.QueryRow(`SELECT compaction_prompt FROM settings WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("read raw compaction prompt: %v", err)
	}
	if stored.Valid {
		t.Errorf("raw compaction prompt = %q, want NULL", stored.String)
	}
}

func TestSaveCompactionPromptPreservesExactCustomText(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	custom := "\n  Summarize with account rules.  \n\nSecond paragraph.\n"
	saveCompactionPromptForTest(t, ctx, database, custom)

	prompt, err := GetCompactionPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Errorf("GetCompactionPrompt() = %q, want exact custom %q", prompt, custom)
	}
}

func TestSaveCompactionPromptBlankOrDefaultStoresNull(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveCompactionPromptForTest(t, ctx, database, "Custom first.")

	for _, value := range []string{"", " \t\n ", "  " + llm.DefaultCompactionPrompt() + "\n"} {
		saveCompactionPromptForTest(t, ctx, database, value)
		var stored sql.NullString
		if err := database.QueryRow(`SELECT compaction_prompt FROM settings WHERE id = 1`).Scan(&stored); err != nil {
			t.Fatalf("read raw compaction prompt after %q: %v", value, err)
		}
		if stored.Valid {
			t.Errorf("raw compaction prompt after %q = %q, want NULL", value, stored.String)
		}
		prompt, err := GetCompactionPrompt(ctx, database)
		if err != nil {
			t.Fatalf("GetCompactionPrompt() after %q error: %v", value, err)
		}
		if prompt != llm.DefaultCompactionPrompt() {
			t.Errorf("GetCompactionPrompt() after %q = %q, want default", value, prompt)
		}
	}
}

func TestGetCompactionPromptExactStoredDefaultResolvesDefault(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`UPDATE settings SET compaction_prompt = ? WHERE id = 1`, llm.DefaultCompactionPrompt()); err != nil {
		t.Fatalf("store exact default: %v", err)
	}
	prompt, err := GetCompactionPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != llm.DefaultCompactionPrompt() {
		t.Errorf("GetCompactionPrompt() = %q, want default", prompt)
	}
}

func TestSaveSettingsOmittedCompactionPromptIsPreserved(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveCompactionPromptForTest(t, ctx, database, "Preserved custom prompt.")

	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "other-model",
		MaxToolRounds: 7,
	}); err != nil {
		t.Fatalf("SaveSettings(without compaction prompt) error: %v", err)
	}
	prompt, err := GetCompactionPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != "Preserved custom prompt." {
		t.Errorf("GetCompactionPrompt() = %q, want preserved custom prompt", prompt)
	}
}

func TestCompactionPromptIsAccountScoped(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	other, err := CreateUser(ctx, database, "CompactionOwner", "compaction-owner-hash")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	otherCtx := WithUser(ctx, other.ID)

	saveCompactionPromptForTest(t, ctx, database, "Legacy account prompt.")
	saveCompactionPromptForTest(t, otherCtx, database, "Other account prompt.")

	legacyPrompt, err := GetCompactionPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt(legacy) error: %v", err)
	}
	if legacyPrompt != "Legacy account prompt." {
		t.Errorf("legacy prompt = %q, want legacy custom prompt", legacyPrompt)
	}
	otherPrompt, err := GetCompactionPrompt(otherCtx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt(other) error: %v", err)
	}
	if otherPrompt != "Other account prompt." {
		t.Errorf("other prompt = %q, want other account custom prompt", otherPrompt)
	}
}

func TestCompactionPromptCiphertextBindsColumnAndOwner(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	storage := newTestStorageCipher(t)
	storageCtx := WithStorageCipher(WithUser(ctx, LegacyOwnerID), storage)
	saveCompactionPromptForTest(t, storageCtx, database, "Bound compaction prompt.")

	var stored string
	if err := database.QueryRow(`SELECT compaction_prompt FROM settings WHERE id = ?`, LegacyOwnerID).Scan(&stored); err != nil {
		t.Fatalf("read stored compaction prompt: %v", err)
	}
	if !strings.HasPrefix(stored, storageTextPrefix) {
		t.Errorf("stored compaction prompt = %q, want encrypted text envelope", stored)
	}
	if _, err := DecryptStorageText(WithStorageCipher(WithUser(ctx, 2), storage), "settings.compaction_prompt", "1", stored); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageText(different owner) error = %v, want ErrStorageDecryption", err)
	}
	if _, err := DecryptStorageText(storageCtx, "settings.system_prompt", "1", stored); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageText(different column) error = %v, want ErrStorageDecryption", err)
	}
	if _, err := DecryptStorageText(storageCtx, "settings.compaction_prompt", "2", stored); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageText(different row) error = %v, want ErrStorageDecryption", err)
	}
}

func TestValidateCompactionPromptSizeLimit(t *testing.T) {
	if err := ValidateCompactionPrompt(strings.Repeat("a", MaxCompactionPromptBytes)); err != nil {
		t.Errorf("ValidateCompactionPrompt(max) error = %v, want nil", err)
	}
	if err := ValidateCompactionPrompt(strings.Repeat("a", MaxCompactionPromptBytes+1)); err == nil {
		t.Error("ValidateCompactionPrompt(over limit) error = nil, want error")
	}
}

func TestSaveSettingsCompactionPromptFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	saveCompactionPromptForTest(t, ctx, database, "Original prompt.")

	if _, err := database.Exec(`
		CREATE TRIGGER fail_compaction_prompt_update
		BEFORE UPDATE OF compaction_prompt ON settings
		BEGIN
			SELECT RAISE(ABORT, 'injected compaction prompt failure');
		END;
	`); err != nil {
		t.Fatalf("inject failure trigger: %v", err)
	}

	changed := "Changed prompt."
	err := SaveSettings(ctx, database, SettingsUpdate{
		Model:            "changed-model",
		MaxToolRounds:    9,
		CompactionPrompt: &changed,
	})
	if err == nil {
		t.Fatal("SaveSettings() error = nil, want injected compaction prompt failure")
	}
	if model, getErr := GetDefaultModel(ctx, database, "fallback"); getErr != nil {
		t.Fatalf("GetDefaultModel() error: %v", getErr)
	} else if model != "test-model" {
		t.Errorf("default model after rollback = %q, want test-model", model)
	}
	prompt, err := GetCompactionPrompt(ctx, database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != "Original prompt." {
		t.Errorf("compaction prompt after rollback = %q, want original", prompt)
	}
}
