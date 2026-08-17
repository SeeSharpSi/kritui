package kritui_db

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestDefaultPromptAppendsReadEmbeddedMarkdown(t *testing.T) {
	values := DefaultPromptAppends()
	if len(values) != 2 {
		t.Fatalf("default prompt append count = %d, want 2", len(values))
	}

	if values[0].ID != "link-check" || values[0].Name != "link check" || values[0].Text == "" {
		t.Errorf("link check preset = %#v", values[0])
	}
	if values[1].ID != "research" || values[1].Name != "research" || values[1].Text == "" {
		t.Errorf("research preset = %#v", values[1])
	}
	for _, value := range values {
		if value.EnabledByDefault {
			t.Errorf("default prompt append %q is enabled by default", value.ID)
		}
	}
}

func TestGetPromptAppendsFallsBackToEmbeddedDefaults(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	values, err := GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	}
	if !slices.Equal(values, DefaultPromptAppends()) {
		t.Errorf("prompt appends = %#v, want embedded defaults", values)
	}
}

func TestSetAndGetPromptAppends(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	want := []PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction.", EnabledByDefault: true}}
	if err := SetPromptAppends(context.Background(), database, want); err != nil {
		t.Fatalf("SetPromptAppends() error: %v", err)
	}
	got, err := GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("prompt appends = %#v, want %#v", got, want)
	}
}

func TestGetPromptAppendsTreatsLegacyValuesAsDisabledByDefault(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.ExecContext(context.Background(), `
		INSERT INTO settings (name, value) VALUES (?, ?)
	`, promptAppendsSetting, `[{"id":"custom","name":"Custom","text":"Use custom instruction."}]`); err != nil {
		t.Fatalf("insert legacy value: %v", err)
	}

	values, err := GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	}
	if len(values) != 1 || values[0].EnabledByDefault {
		t.Errorf("legacy prompt appends = %#v, want one disabled append", values)
	}
}

func TestGetPromptAppendsRejectsCorruptValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{
			name:  "invalid json",
			value: "not-json",
		},
		{
			name:  "fails validation",
			value: `[{"id":"","name":"x","text":"y"}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := openMessagesTestDatabase(t, "")
			if _, err := database.ExecContext(context.Background(), `
				INSERT INTO settings (name, value) VALUES (?, ?)
			`, promptAppendsSetting, tt.value); err != nil {
				t.Fatalf("insert corrupt value: %v", err)
			}
			values, err := GetPromptAppends(context.Background(), database)
			if err == nil {
				t.Fatal("GetPromptAppends() error = nil, want an error")
			}
			if values != nil {
				t.Errorf("GetPromptAppends() values = %#v, want nil", values)
			}
		})
	}
}

func TestSetPromptAppendsPrunesRemovedChatSelections(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	chatA, err := InsertChat(ctx, database, "A", nil, []string{"keep", "remove", "keep-too"})
	if err != nil {
		t.Fatalf("InsertChat(A): %v", err)
	}
	chatB, err := InsertChat(ctx, database, "B", nil, []string{"remove"})
	if err != nil {
		t.Fatalf("InsertChat(B): %v", err)
	}

	values := []PromptAppend{
		{ID: "keep", Name: "Keep", Text: "Keep it."},
		{ID: "keep-too", Name: "Keep too", Text: "Keep too."},
	}
	if err := SetPromptAppends(ctx, database, values); err != nil {
		t.Fatalf("SetPromptAppends() error: %v", err)
	}

	gotA, err := GetChatPromptAppendIDs(ctx, database, chatA)
	if err != nil {
		t.Fatalf("GetChatPromptAppendIDs(A): %v", err)
	}
	if !slices.Equal(gotA, []string{"keep", "keep-too"}) {
		t.Errorf("chat A prompt append IDs = %#v, want [keep keep-too]", gotA)
	}

	gotB, err := GetChatPromptAppendIDs(ctx, database, chatB)
	if err != nil {
		t.Fatalf("GetChatPromptAppendIDs(B): %v", err)
	}
	if len(gotB) != 0 {
		t.Errorf("chat B prompt append IDs = %#v, want empty", gotB)
	}

	if settings, err := GetPromptAppends(ctx, database); err != nil {
		t.Fatalf("GetPromptAppends() error: %v", err)
	} else if !slices.Equal(settings, values) {
		t.Errorf("prompt appends = %#v, want %#v", settings, values)
	}
}

func TestPromptAppendIDsPreservesOrder(t *testing.T) {
	values := []PromptAppend{
		{ID: "second", Name: "Second", Text: "Second."},
		{ID: "first", Name: "First", Text: "First."},
		{ID: "third", Name: "Third", Text: "Third."},
	}
	ids := PromptAppendIDs(values)
	if !slices.Equal(ids, []string{"second", "first", "third"}) {
		t.Errorf("PromptAppendIDs() = %#v, want [second first third]", ids)
	}

	empty := PromptAppendIDs(nil)
	if empty == nil {
		t.Error("PromptAppendIDs(nil) = nil, want non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Errorf("PromptAppendIDs(nil) length = %d, want 0", len(empty))
	}
}

func TestDefaultPromptAppendIDsPreservesEnabledOrder(t *testing.T) {
	values := []PromptAppend{
		{ID: "second", EnabledByDefault: true},
		{ID: "first"},
		{ID: "third", EnabledByDefault: true},
	}
	ids := DefaultPromptAppendIDs(values)
	if !slices.Equal(ids, []string{"second", "third"}) {
		t.Errorf("DefaultPromptAppendIDs() = %#v, want [second third]", ids)
	}

	empty := DefaultPromptAppendIDs(nil)
	if empty == nil {
		t.Error("DefaultPromptAppendIDs(nil) = nil, want non-nil empty slice")
	}
}

func TestSelectPromptAppendsRejectsUnknownAndRepeatedIDs(t *testing.T) {
	values := DefaultPromptAppends()
	if _, err := SelectPromptAppends(values, []string{"missing"}); err == nil {
		t.Fatal("SelectPromptAppends() unknown ID error = nil")
	}
	if _, err := SelectPromptAppends(values, []string{"research", "research"}); err == nil {
		t.Fatal("SelectPromptAppends() repeated ID error = nil")
	}
}

func TestValidatePromptAppendID(t *testing.T) {
	valid64 := strings.Repeat("a", 64)
	over64 := strings.Repeat("a", 65)

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "empty", id: "", want: false},
		{name: "whitespace only", id: "   ", want: false},
		{name: "interior whitespace", id: "ab cd", want: false},
		{name: "single alphanumeric", id: "a", want: true},
		{name: "single digit", id: "7", want: true},
		{name: "leading hyphen", id: "-ab", want: false},
		{name: "trailing hyphen", id: "ab-", want: false},
		{name: "interior hyphen", id: "ab-cd", want: true},
		{name: "uppercase", id: "Append", want: false},
		{name: "underscore", id: "a_b", want: false},
		{name: "punctuation", id: "a.b!", want: false},
		{name: "non-ASCII", id: "append-\u00e9", want: false},
		{name: "generated id", id: "append-0123456789abcdef0123", want: true},
		{name: "built-in link-check", id: "link-check", want: true},
		{name: "built-in research", id: "research", want: true},
		{name: "exactly 64 bytes", id: valid64, want: true},
		{name: "65 bytes", id: over64, want: false},
		{name: "digits with hyphen", id: "a-1-b", want: true},
		{name: "hyphen only", id: "-", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePromptAppendID(test.id)
			if (err == nil) != test.want {
				t.Errorf("ValidatePromptAppendID(%q) error = %v, want valid=%v", test.id, err, test.want)
			}
		})
	}

	if err := ValidatePromptAppendID(over64); err == nil {
		t.Fatal("ValidatePromptAppendID(65 bytes) error = nil")
	} else if !strings.Contains(err.Error(), "64") {
		t.Errorf("ValidatePromptAppendID(65 bytes) error = %v, want the byte limit in the message", err)
	}
	if err := ValidatePromptAppendID("bad char!"); err == nil {
		t.Fatal("ValidatePromptAppendID(invalid char) error = nil")
	} else if !strings.Contains(err.Error(), "bad char!") {
		t.Errorf("ValidatePromptAppendID(invalid char) error = %q, want offending ID in the message", err)
	}
}

func TestPromptAppendIDValidationAppliesToAllAccessors(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	malformed := []PromptAppend{{ID: "bad char", Name: "Bad", Text: "Bad."}}
	if err := SetPromptAppends(ctx, database, malformed); err == nil {
		t.Error("SetPromptAppends() malformed ID error = nil")
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "model",
		MaxToolRounds: 8,
		DefaultTools:  []string{"webfetch"},
		PromptAppends: malformed,
	}); err == nil {
		t.Error("SaveSettings() malformed ID error = nil")
	}
	if err := ValidatePromptAppends(malformed); err == nil {
		t.Error("ValidatePromptAppends() malformed ID error = nil")
	}

	if err := SetPromptAppends(ctx, database, []PromptAppend{{ID: "ok", Name: "Ok", Text: "Fine."}}); err != nil {
		t.Fatalf("seed SetPromptAppends() error: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE settings SET value = '[
			{"id":"bad char","name":"Bad","text":"Bad."},
			{"id":"ok","name":"Ok","text":"Fine."}
		]' WHERE name = 'prompt_appends'
	`); err != nil {
		t.Fatalf("corrupt stored appends: %v", err)
	}
	if _, err := GetPromptAppends(ctx, database); err == nil {
		t.Error("GetPromptAppends() malformed stored ID error = nil")
	}
}
