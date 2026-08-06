package kritui_db

import (
	"context"
	"testing"
)

func TestUpsertChatInsertEncodesEmptySlices(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	if err := UpsertChat(ctx, database, 1, "first title", nil, nil); err != nil {
		t.Fatalf("UpsertChat(nil tools/appendIDs) error: %v", err)
	}

	var title string
	var tools, appends string
	if err := database.QueryRow(`SELECT title, tools, appends FROM chats WHERE id = 1`).Scan(&title, &tools, &appends); err != nil {
		t.Fatalf("query inserted chat: %v", err)
	}
	if title != "first title" {
		t.Errorf("title = %q, want %q", title, "first title")
	}
	if tools != "[]" {
		t.Errorf("tools = %q, want []", tools)
	}
	if appends != "[]" {
		t.Errorf("appends = %q, want []", appends)
	}
}

func TestUpsertChatPreservesNonEmptyTitleAndReplacesToolsAppends(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	if err := UpsertChat(ctx, database, 1, "first title", []string{"lookup"}, []string{"one", "two"}); err != nil {
		t.Fatalf("UpsertChat(insert) error: %v", err)
	}
	if err := UpsertChat(ctx, database, 1, "new title", []string{"webfetch", "git"}, []string{"three"}); err != nil {
		t.Fatalf("UpsertChat(conflict) error: %v", err)
	}

	var title string
	var tools, appends string
	if err := database.QueryRow(`SELECT title, tools, appends FROM chats WHERE id = 1`).Scan(&title, &tools, &appends); err != nil {
		t.Fatalf("query updated chat: %v", err)
	}
	if title != "first title" {
		t.Errorf("title = %q, want preserved %q", title, "first title")
	}
	if tools != `["webfetch","git"]` {
		t.Errorf("tools = %q, want replaced %q", tools, `["webfetch","git"]`)
	}
	if appends != `["three"]` {
		t.Errorf("appends = %q, want replaced %q", appends, `["three"]`)
	}
}

func TestUpsertChatFillsEmptyExistingTitle(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (2, '')`); err != nil {
		t.Fatalf("insert empty-title chat: %v", err)
	}
	if err := UpsertChat(ctx, database, 2, "initial title", nil, nil); err != nil {
		t.Fatalf("UpsertChat over empty title error: %v", err)
	}
	var storedTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 2`).Scan(&storedTitle); err != nil {
		t.Fatalf("query filled chat: %v", err)
	}
	if storedTitle != "initial title" {
		t.Errorf("empty title replaced with = %q, want %q", storedTitle, "initial title")
	}
}

func TestUpsertChatRunsInsideTransaction(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	if err := UpsertChat(ctx, tx, 1, "tx title", []string{"lookup"}, []string{"one"}); err != nil {
		t.Fatalf("UpsertChat(*sql.Tx) error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats WHERE id = 1`).Scan(&count); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if count != 1 {
		t.Errorf("committed chat count = %d, want 1", count)
	}
}
