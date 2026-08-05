package kritui_db

import (
	"context"
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
