package kritui_db

import (
	"context"
	"slices"
	"testing"
)

func TestGetMaxToolRoundsFallsBackWhenUnset(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	rounds, err := GetMaxToolRounds(context.Background(), database, 7)
	if err != nil {
		t.Fatalf("GetMaxToolRounds() error: %v", err)
	}
	if rounds != 7 {
		t.Errorf("max tool rounds = %d, want 7", rounds)
	}
}

func TestSetAndGetMaxToolRounds(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if err := SetMaxToolRounds(context.Background(), database, 42); err != nil {
		t.Fatalf("SetMaxToolRounds() error: %v", err)
	}
	rounds, err := GetMaxToolRounds(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("GetMaxToolRounds() error: %v", err)
	}
	if rounds != 42 {
		t.Errorf("max tool rounds = %d, want 42", rounds)
	}
}

func TestSetMaxToolRoundsRejectsInvalidValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	for _, rounds := range []int{0, -1, MaxConfigurableToolRounds + 1} {
		if err := SetMaxToolRounds(context.Background(), database, rounds); err == nil {
			t.Errorf("SetMaxToolRounds(%d) error = nil, want error", rounds)
		}
	}
}

func TestGetMaxToolRoundsFallsBackWhenValueInvalid(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	for _, value := range []string{"garbage", "0", "-3"} {
		if _, err := database.Exec(`INSERT INTO settings (name, value) VALUES (?, ?)`, maxToolRoundsSetting, value); err != nil {
			t.Fatalf("insert invalid setting: %v", err)
		}
		rounds, err := GetMaxToolRounds(context.Background(), database, 9)
		if err != nil {
			t.Fatalf("GetMaxToolRounds() error: %v", err)
		}
		if rounds != 9 {
			t.Errorf("max tool rounds for %q = %d, want fallback 9", value, rounds)
		}
		if _, err := database.Exec(`DELETE FROM settings WHERE name = ?`, maxToolRoundsSetting); err != nil {
			t.Fatalf("clean up setting: %v", err)
		}
	}
}

func TestGetDefaultEnabledToolsFallsBackWhenUnset(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	names, err := GetDefaultEnabledTools(context.Background(), database, []string{"webfetch"})
	if err != nil {
		t.Fatalf("GetDefaultEnabledTools() error: %v", err)
	}
	if len(names) != 1 || names[0] != "webfetch" {
		t.Errorf("default enabled tools = %v, want [webfetch]", names)
	}
}

func TestSetAndGetDefaultEnabledTools(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if err := SetDefaultEnabledTools(context.Background(), database, []string{"webfetch", "git"}); err != nil {
		t.Fatalf("SetDefaultEnabledTools() error: %v", err)
	}
	names, err := GetDefaultEnabledTools(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("GetDefaultEnabledTools() error: %v", err)
	}
	if len(names) != 2 || names[0] != "webfetch" || names[1] != "git" {
		t.Errorf("default enabled tools = %v, want [webfetch git]", names)
	}
}

func TestSetDefaultEnabledToolsReplacesValue(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if err := SetDefaultEnabledTools(context.Background(), database, []string{"webfetch"}); err != nil {
		t.Fatalf("SetDefaultEnabledTools() error: %v", err)
	}
	if err := SetDefaultEnabledTools(context.Background(), database, []string{}); err != nil {
		t.Fatalf("SetDefaultEnabledTools() error: %v", err)
	}
	names, err := GetDefaultEnabledTools(context.Background(), database, []string{"fallback"})
	if err != nil {
		t.Fatalf("GetDefaultEnabledTools() error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("default enabled tools = %v, want empty list", names)
	}
}

func TestGetDefaultEnabledToolsFallsBackWhenValueInvalid(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	for _, value := range []string{"garbage", `{"name":"webfetch"}`} {
		if _, err := database.Exec(`INSERT INTO settings (name, value) VALUES (?, ?)`, defaultToolsSetting, value); err != nil {
			t.Fatalf("insert invalid setting: %v", err)
		}
		names, err := GetDefaultEnabledTools(context.Background(), database, []string{"git"})
		if err != nil {
			t.Fatalf("GetDefaultEnabledTools() error: %v", err)
		}
		if len(names) != 1 || names[0] != "git" {
			t.Errorf("default enabled tools for %q = %v, want fallback [git]", value, names)
		}
		if _, err := database.Exec(`DELETE FROM settings WHERE name = ?`, defaultToolsSetting); err != nil {
			t.Fatalf("clean up setting: %v", err)
		}
	}
}

func TestSaveSettingsRollsBackWhenPromptAppendsWriteFails(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	oldAppends := []PromptAppend{
		{ID: "keep", Name: "Keep", Text: "Keep it."},
		{ID: "gone", Name: "Gone", Text: "Gone."},
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "old-model",
		MaxToolRounds: 3,
		DefaultTools:  []string{"git"},
		PromptAppends: oldAppends,
	}); err != nil {
		t.Fatalf("seed SaveSettings() error: %v", err)
	}
	chatID, err := InsertChat(ctx, database, "rollback chat", []string{"git"}, []string{"keep", "gone"})
	if err != nil {
		t.Fatalf("InsertChat() error: %v", err)
	}

	// Model, rounds, and tools upserts execute first inside the same
	// transaction. A trigger that raises only for the prompt_appends settings
	// write forces the final write to fail, exercising rollback after earlier
	// values were already changed but never committed.
	if _, err := database.Exec(`
		CREATE TRIGGER fail_prompt_appends_insert
		AFTER INSERT ON settings
		WHEN NEW.name = 'prompt_appends'
		BEGIN
			SELECT RAISE(ABORT, 'injected prompt appends failure');
		END;
		CREATE TRIGGER fail_prompt_appends_update
		AFTER UPDATE ON settings
		WHEN OLD.name = 'prompt_appends'
		BEGIN
			SELECT RAISE(ABORT, 'injected prompt appends failure');
		END;
	`); err != nil {
		t.Fatalf("inject failure triggers: %v", err)
	}

	err = SaveSettings(ctx, database, SettingsUpdate{
		Model:         "new-model",
		MaxToolRounds: 9,
		DefaultTools:  []string{"webfetch"},
		PromptAppends: []PromptAppend{{ID: "new", Name: "New", Text: "New instruction."}},
	})
	if err == nil {
		t.Fatal("SaveSettings() error = nil, want injected prompt appends failure")
	}

	if got, err := GetDefaultModel(ctx, database, "fallback"); err != nil {
		t.Fatalf("GetDefaultModel() error: %v", err)
	} else if got != "old-model" {
		t.Errorf("default model after rollback = %q, want old-model", got)
	}
	if got, err := GetMaxToolRounds(ctx, database, 1); err != nil {
		t.Fatalf("GetMaxToolRounds() error: %v", err)
	} else if got != 3 {
		t.Errorf("max tool rounds after rollback = %d, want 3", got)
	}
	if got, err := GetDefaultEnabledTools(ctx, database, nil); err != nil {
		t.Fatalf("GetDefaultEnabledTools() error: %v", err)
	} else if !slices.Equal(got, []string{"git"}) {
		t.Errorf("default enabled tools after rollback = %v, want [git]", got)
	}
	if got, err := GetPromptAppends(ctx, database); err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	} else if !slices.Equal(got, oldAppends) {
		t.Errorf("prompt appends after rollback = %#v, want %#v", got, oldAppends)
	}
	if got, err := GetChatPromptAppendIDs(ctx, database, chatID); err != nil {
		t.Fatalf("GetChatPromptAppendIDs() error: %v", err)
	} else if !slices.Equal(got, []string{"keep", "gone"}) {
		t.Errorf("chat prompt append selections after rollback = %v, want [keep gone]", got)
	}
}

func TestSaveSettingsPreservesPromptAppendsWhenOmitted(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	appends := []PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction."}}
	if err := SetPromptAppends(ctx, database, appends); err != nil {
		t.Fatalf("SetPromptAppends() error: %v", err)
	}

	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 12,
		DefaultTools:  []string{"webfetch"},
	}); err != nil {
		t.Fatalf("SaveSettings() error: %v", err)
	}

	got, err := GetPromptAppends(ctx, database)
	if err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	}
	if !slices.Equal(got, appends) {
		t.Errorf("prompt appends = %#v, want preserved %#v", got, appends)
	}
	for name, want := range map[string]string{
		"default_model":   "saved-model",
		"max_tool_rounds": "12",
	} {
		var value string
		if err := database.QueryRow(`SELECT value FROM settings WHERE name = ?`, name).Scan(&value); err != nil {
			t.Fatalf("get setting %q: %v", name, err)
		}
		if value != want {
			t.Errorf("setting %q = %q, want %q", name, value, want)
		}
	}
}
