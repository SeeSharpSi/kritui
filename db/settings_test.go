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

func TestMaxToolRoundsColumnRejectsInvalidValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	for _, rounds := range []int{0, -3, MaxConfigurableToolRounds + 1} {
		if _, err := database.Exec(`UPDATE settings SET max_tool_rounds = ? WHERE id = 1`, rounds); err == nil {
			t.Errorf("store invalid max tool rounds %d error = nil", rounds)
		}
	}
}

func TestGetDefaultEnabledToolsFallsBackWhenUnset(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	names, err := GetDefaultEnabledTools(context.Background(), database, []string{"webfetch"})
	if err != nil {
		t.Fatalf("GetDefaultEnabledTools() error: %v", err)
	}
	if !slices.Equal(names, []string{"webfetch"}) {
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
	if !slices.Equal(names, []string{"webfetch", "git"}) {
		t.Errorf("default enabled tools = %v, want [webfetch git]", names)
	}
}

func TestSetDefaultEnabledToolsStoresOrderedRows(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if err := SetDefaultEnabledTools(context.Background(), database, []string{"webfetch", "git"}); err != nil {
		t.Fatalf("SetDefaultEnabledTools() error: %v", err)
	}
	rows, err := database.Query(`SELECT position, name FROM default_tools ORDER BY position`)
	if err != nil {
		t.Fatalf("query default tools: %v", err)
	}
	defer rows.Close()
	for position, want := range []string{"webfetch", "git"} {
		if !rows.Next() {
			t.Fatalf("default tool row %d missing", position)
		}
		var gotPosition int
		var gotName string
		if err := rows.Scan(&gotPosition, &gotName); err != nil {
			t.Fatalf("scan default tool: %v", err)
		}
		if gotPosition != position || gotName != want {
			t.Errorf("default tool row = (%d, %q), want (%d, %q)", gotPosition, gotName, position, want)
		}
	}
}

func TestSetDefaultEnabledToolsReplacesWithExplicitEmptyList(t *testing.T) {
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

	if _, err := database.Exec(`
		CREATE TRIGGER fail_prompt_appends_insert
		BEFORE INSERT ON prompt_appends
		WHEN NEW.id = 'new'
		BEGIN
			SELECT RAISE(ABORT, 'injected prompt appends failure');
		END;
	`); err != nil {
		t.Fatalf("inject failure trigger: %v", err)
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
	var model string
	var rounds int
	if err := database.QueryRow(`SELECT default_model, max_tool_rounds FROM settings WHERE id = 1`).Scan(&model, &rounds); err != nil {
		t.Fatalf("get typed settings: %v", err)
	}
	if model != "saved-model" || rounds != 12 {
		t.Errorf("typed settings = (%q, %d), want (saved-model, 12)", model, rounds)
	}
}
