package kritui_db

import (
	"context"
	"testing"
)

func TestUpsertChatInsertStoresEmptyOptionSets(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")

	if err := UpsertChat(ctx, database, 1, "first title", nil, nil); err != nil {
		t.Fatalf("UpsertChat(nil tools/appendIDs) error: %v", err)
	}

	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&title); err != nil {
		t.Fatalf("query inserted chat: %v", err)
	}
	if title != "first title" {
		t.Errorf("title = %q, want %q", title, "first title")
	}
	tools, err := GetChatTools(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetChatTools() error: %v", err)
	}
	appends, err := GetChatPromptAppendIDs(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetChatPromptAppendIDs() error: %v", err)
	}
	if len(tools) != 0 || len(appends) != 0 {
		t.Errorf("stored options = tools %v, appends %v; want empty", tools, appends)
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
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&title); err != nil {
		t.Fatalf("query updated chat: %v", err)
	}
	if title != "first title" {
		t.Errorf("title = %q, want preserved %q", title, "first title")
	}
	tools, err := GetChatTools(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetChatTools() error: %v", err)
	}
	appends, err := GetChatPromptAppendIDs(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetChatPromptAppendIDs() error: %v", err)
	}
	if len(tools) != 2 || tools[0] != "webfetch" || tools[1] != "git" {
		t.Errorf("tools = %v, want [webfetch git]", tools)
	}
	if len(appends) != 1 || appends[0] != "three" {
		t.Errorf("appends = %v, want [three]", appends)
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
