package kritui_db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"seesharpsi/kritui/llm"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var messagesTestSchema string

func TestAppendCompletionUsesStoredLastPosition(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'first'),
			(1, 2, 'user', 'second');
	`); err != nil {
		t.Fatalf("insert gapped messages: %v", err)
	}

	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{
		{Role: "assistant", Content: "first response"},
		{Role: "assistant", Content: "second response"},
	})
	if err != nil {
		t.Fatalf("append completion: %v", err)
	}

	rows, err := database.Query(`SELECT position FROM messages WHERE chat_id = 1 ORDER BY position`)
	if err != nil {
		t.Fatalf("query positions: %v", err)
	}
	defer rows.Close()
	var positions []int
	for rows.Next() {
		var position int
		if err := rows.Scan(&position); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate positions: %v", err)
	}
	want := []int{0, 2, 3, 4}
	if !slices.Equal(positions, want) {
		t.Errorf("positions = %v, want %v", positions, want)
	}
}

func TestAppendCompletionRejectsChangedConversation(t *testing.T) {
	database, other := openSharedMessagesTestDatabases(t)
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	if _, err := InsertMessage(context.Background(), other, 1, 1, llm.Message{Role: "user", Content: "changed"}); err != nil {
		t.Fatalf("change conversation: %v", err)
	}

	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale"}})
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("AppendCompletion() error = %v, want conversation conflict", err)
	}
	var generated int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE content = 'stale'`).Scan(&generated); err != nil {
		t.Fatalf("count generated messages: %v", err)
	}
	if generated != 0 {
		t.Errorf("stored stale message count = %d, want 0", generated)
	}
}

func TestAppendCompletionRejectsEditedConversation(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	if _, err := database.Exec(`UPDATE messages SET content = 'edited' WHERE chat_id = 1`); err != nil {
		t.Fatalf("edit conversation: %v", err)
	}

	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale"}})
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("AppendCompletion() error = %v, want conversation conflict", err)
	}
}

func TestAppendCompletionSerializesConcurrentWriters(t *testing.T) {
	database, other := openSharedMessagesTestDatabases(t)
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}

	start := make(chan struct{})
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for index, connection := range []*sql.DB{database, other} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsChannel <- AppendCompletion(context.Background(), connection, 1, snapshot, []llm.Message{{
				Role:    "assistant",
				Content: fmt.Sprintf("response %d", index),
			}})
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)

	var succeeded, conflicted int
	for err := range errorsChannel {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrConversationConflict):
			conflicted++
		default:
			t.Errorf("AppendCompletion() error = %v, want nil or conflict", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Errorf("append results = %d succeeded, %d conflicted; want 1 each", succeeded, conflicted)
	}

	var count, maximumPosition int
	if err := database.QueryRow(`SELECT COUNT(*), MAX(position) FROM messages WHERE chat_id = 1`).Scan(&count, &maximumPosition); err != nil {
		t.Fatalf("query stored messages: %v", err)
	}
	if count != 2 || maximumPosition != 1 {
		t.Errorf("stored messages = count %d, max position %d; want 2 and 1", count, maximumPosition)
	}
}

func TestAppendCompletionDetectsDeletedChat(t *testing.T) {
	database, other := openSharedMessagesTestDatabases(t)
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	if _, err := other.Exec(`DELETE FROM chats WHERE id = 1`); err != nil {
		t.Fatalf("delete chat: %v", err)
	}

	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale"}})
	if !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("AppendCompletion() error = %v, want chat not found", err)
	}
}

func TestAppendCompletionRollsBackPartialWrite(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}

	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{
		{Role: "assistant", Content: "valid"},
		{Role: "invalid", Content: "invalid"},
	})
	if err == nil {
		t.Fatal("AppendCompletion() error = nil, want insert error")
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1`).Scan(&count); err != nil {
		t.Fatalf("count stored messages: %v", err)
	}
	if count != 1 {
		t.Errorf("stored message count = %d, want original message only", count)
	}
}

func openSharedMessagesTestDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	database := openMessagesTestDatabase(t, path)
	other := openMessagesTestConnection(t, path)
	return database, other
}

func openMessagesTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	database := openMessagesTestConnection(t, path)
	if _, err := database.Exec(messagesTestSchema); err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	return database
}

func openMessagesTestConnection(t *testing.T, path string) *sql.DB {
	t.Helper()
	dataSourceName := ":memory:"
	if path != "" {
		parameters := url.Values{}
		parameters.Add("_pragma", "foreign_keys(1)")
		parameters.Add("_pragma", "busy_timeout(5000)")
		dataSourceName = (&url.URL{Scheme: "file", Opaque: path, RawQuery: parameters.Encode()}).String()
	}
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

func insertMessagesTestUser(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'question');
	`); err != nil {
		t.Fatalf("insert user message: %v", err)
	}
}
