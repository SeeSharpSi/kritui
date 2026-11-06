package kritui_db

import (
	"context"
	"path/filepath"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestReasoningPreferences(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	high, low := "high", "low"
	update := SettingsUpdate{Model: "test", MaxToolRounds: 16, ReasoningEffort: &high, TitleReasoningEffort: &low}
	if err := SaveSettings(ctx, database, update); err != nil {
		t.Fatal(err)
	}
	if got, err := GetChatReasoningEffort(ctx, database, 1); err != nil || got != high {
		t.Fatalf("inherited effort = %q, %v", got, err)
	}
	if err := SetChatReasoningEffort(ctx, database, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := GetChatReasoningEffort(ctx, database, 1); err != nil || got != "" {
		t.Fatalf("explicit Default = %q, %v", got, err)
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{Model: "test", MaxToolRounds: 16}); err != nil {
		t.Fatal(err)
	}
	if got, err := GetDefaultReasoningEffort(ctx, database); err != nil || got != high {
		t.Fatalf("preserved default = %q, %v", got, err)
	}
	if got, err := GetTitleGenerationSettings(ctx, database); err != nil || got.ReasoningEffort != low {
		t.Fatalf("independent title effort = %#v, %v", got, err)
	}
	if _, err := GetChatReasoningEffort(WithUser(ctx, 999), database, 1); err == nil {
		t.Fatal("foreign account read chat effort")
	}
}

func TestInboxReasoningSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	database := openMessagesTestDatabase(t, path)
	ctx := context.Background()
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	active := CompletionState{Pending: true, Model: "active", ReasoningEffort: "high"}
	if err := SetCompletionState(ctx, database, 1, active); err != nil {
		t.Fatal(err)
	}
	for _, item := range []InboxItem{
		{SubmissionID: "steer", Delivery: "steer", Model: "other", ReasoningEffort: "low", Message: llm.Message{Role: "user", Content: "Steer"}},
		{SubmissionID: "queue", Delivery: "queue", Model: "queued", ReasoningEffort: "medium", Message: llm.Message{Role: "user", Content: "Queued"}},
	} {
		stored := enqueueInboxTestItem(t, database, ctx, 1, item)
		if stored.ReasoningEffort != item.ReasoningEffort {
			t.Fatalf("enqueue lost effort: %#v", stored)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openMessagesTestConnection(t, path)
	items, err := GetInbox(ctx, database, 1)
	if err != nil || len(items) != 2 || items[0].ReasoningEffort != "low" || items[1].ReasoningEffort != "medium" {
		t.Fatalf("reopened inbox = %#v, %v", items, err)
	}
	for _, want := range []CompletionState{active, {Pending: true, Model: "queued", ReasoningEffort: "medium"}} {
		snapshot, err := GetMessageSnapshot(ctx, database, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CheckpointCompletionWithState(ctx, database, 1, snapshot, nil, "input", active); err != nil {
			t.Fatal(err)
		}
		got, err := GetCompletionState(ctx, database, 1)
		if err != nil || got.Model != want.Model || got.ReasoningEffort != want.ReasoningEffort {
			t.Fatalf("checkpoint = %#v, %v; want %#v", got, err, want)
		}
	}
	if got, err := GetChatReasoningEffort(ctx, database, 1); err != nil || got != "medium" {
		t.Fatalf("queued chat preference = %q, %v", got, err)
	}
}
