package kritui_db

import (
	"context"
	"slices"
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
	want := []PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction."}}
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

func TestSelectPromptAppendsRejectsUnknownAndRepeatedIDs(t *testing.T) {
	values := DefaultPromptAppends()
	if _, err := SelectPromptAppends(values, []string{"missing"}); err == nil {
		t.Fatal("SelectPromptAppends() unknown ID error = nil")
	}
	if _, err := SelectPromptAppends(values, []string{"research", "research"}); err == nil {
		t.Fatal("SelectPromptAppends() repeated ID error = nil")
	}
}
