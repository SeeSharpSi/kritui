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

	if values[0].ID != "link-check" || values[0].Name != "link check" || values[0].Text != "double check links before sending them to me" {
		t.Errorf("link check preset = %#v", values[0])
	}
	if values[1].ID != "research" || values[1].Name != "research" || values[1].Text != "search at least two primary sources to find answers" {
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

func TestSelectPromptAppendsRejectsUnknownAndRepeatedIDs(t *testing.T) {
	values := DefaultPromptAppends()
	if _, err := SelectPromptAppends(values, []string{"missing"}); err == nil {
		t.Fatal("SelectPromptAppends() unknown ID error = nil")
	}
	if _, err := SelectPromptAppends(values, []string{"research", "research"}); err == nil {
		t.Fatal("SelectPromptAppends() repeated ID error = nil")
	}
}
