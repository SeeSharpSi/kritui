package kritui_db

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
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

func TestMessageImagesRoundTripAndSnapshotConflict(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`)
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.UserImage{{Filename: "b.png", MediaType: "image/png", Width: 2, Height: 3, Data: []byte{1, 2}}, {Filename: "a.jpg", MediaType: "image/jpeg", Data: []byte{3}}}
	if _, err := InsertMessage(context.Background(), database, 1, 0, llm.Message{Role: "user", Images: want}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snapshot.Messages[0].Images[0].Data, want[0].Data) || snapshot.Messages[0].Images[1].Filename != "a.jpg" {
		t.Fatalf("images round trip: %#v", snapshot.Messages[0].Images)
	}
	var typ string
	if err := database.QueryRow(`SELECT typeof(data) FROM message_user_images WHERE position = 0`).Scan(&typ); err != nil || typ != "blob" {
		t.Fatalf("typeof image data = %q, %v", typ, err)
	}
	if _, err := database.Exec(`UPDATE message_user_images SET data = ? WHERE position = 0`, []byte{9}); err != nil {
		t.Fatal(err)
	}
	if err := AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "x"}}); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("image mutation error = %v", err)
	}
}

func TestInsertMessageRejectsInvalidImagesAtomically(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	tests := []llm.Message{
		{Role: "assistant", Images: []llm.UserImage{{MediaType: "x", Data: []byte{1}}}},
		{Role: "user", Images: []llm.UserImage{{MediaType: "", Data: []byte{1}}}},
		{Role: "user", Images: []llm.UserImage{{MediaType: "x"}}},
		{Role: "user", Images: []llm.UserImage{{MediaType: "x", Width: -1, Data: []byte{1}}}},
		{Role: "user", Images: []llm.UserImage{{MediaType: "x", Height: -1, Data: []byte{1}}}},
	}
	for _, message := range tests {
		if _, err := InsertMessage(context.Background(), database, 1, 0, message); err == nil {
			t.Fatalf("invalid image accepted: %#v", message)
		}
		var count int
		if err := database.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("atomic rollback count=%d err=%v", count, err)
		}
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

func TestAppendCompletionRejectsEditedMessageCollections(t *testing.T) {
	metadata, err := llm.NewResponsesProviderMetadata([]json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"reasoning-1"}`)})
	if err != nil {
		t.Fatalf("create provider metadata: %v", err)
	}
	tests := []struct {
		name    string
		message llm.Message
		update  string
	}{
		{
			name: "tool call",
			message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`},
			}}},
			update: `UPDATE message_tool_calls SET arguments = '{"changed":true}'`,
		},
		{
			name:    "prompt append",
			message: llm.Message{Role: "user", Content: "question", PromptAppendTexts: []string{"Original instruction."}},
			update:  `UPDATE message_prompt_appends SET text = 'Changed instruction.'`,
		},
		{
			name:    "prompt append position",
			message: llm.Message{Role: "user", Content: "question", PromptAppendTexts: []string{"Original instruction."}},
			update:  `UPDATE message_prompt_appends SET position = 2`,
		},
		{
			name:    "provider output",
			message: llm.Message{Role: "assistant", ProviderMetadata: metadata},
			update:  `UPDATE message_provider_outputs SET payload = '{"type":"reasoning","id":"reasoning-2"}'`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openMessagesTestDatabase(t, "")
			if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
				t.Fatalf("insert chat: %v", err)
			}
			if _, err := InsertMessage(context.Background(), database, 1, 0, test.message); err != nil {
				t.Fatalf("insert message: %v", err)
			}
			snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
			if err != nil {
				t.Fatalf("get message snapshot: %v", err)
			}
			if _, err := database.Exec(test.update); err != nil {
				t.Fatalf("edit message collection: %v", err)
			}
			err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale"}})
			if !errors.Is(err, ErrConversationConflict) {
				t.Fatalf("AppendCompletion() error = %v, want conversation conflict", err)
			}
		})
	}
}

func TestInsertMessageRollsBackWhenChildInsertFails(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	_, err := InsertMessage(context.Background(), database, 1, 0, llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}},
			{ID: "call-2", Type: "function", Function: llm.FunctionCall{Name: "", Arguments: `{}`}},
		},
	})
	if err == nil {
		t.Fatal("InsertMessage() error = nil, want invalid function name error")
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Errorf("stored message count = %d, want 0 after rollback", count)
	}
}

func TestInsertMessagePreservesOpaqueToolArguments(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 0, llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `opaque-provider-text`},
		}},
	}); err != nil {
		t.Fatalf("InsertMessage() error: %v", err)
	}
	messages, err := GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("GetMessages() error: %v", err)
	}
	if len(messages) != 1 || len(messages[0].ToolCalls) != 1 || messages[0].ToolCalls[0].Function.Arguments != "opaque-provider-text" {
		t.Errorf("stored messages = %#v, want opaque tool arguments preserved", messages)
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

func TestReplaceUserMessageKeepsTargetAndTruncatesLaterMessages(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 0, llm.Message{Role: "user", Content: "earlier"}); err != nil {
		t.Fatalf("insert earlier user: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 1, llm.Message{Role: "assistant", Content: "earlier answer"}); err != nil {
		t.Fatalf("insert earlier assistant: %v", err)
	}
	targetID, err := InsertMessage(context.Background(), database, 1, 2, llm.Message{
		Role:              "user",
		Content:           "original",
		PromptAppendTexts: []string{"old append"},
		Images:            []llm.UserImage{{Filename: "target.png", MediaType: "image/png", Width: 4, Height: 5, Data: []byte{1, 2, 3}}},
	})
	if err != nil {
		t.Fatalf("insert edit target: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 3, llm.Message{
		Role:    "assistant",
		Content: "discarded answer",
		ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`},
		}},
	}); err != nil {
		t.Fatalf("insert discarded assistant: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 4, llm.Message{Role: "tool", Content: "discarded result", ToolCallID: "call-1"}); err != nil {
		t.Fatalf("insert discarded tool: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 5, llm.Message{
		Role:              "user",
		Content:           "discarded follow-up",
		PromptAppendTexts: []string{"discarded append"},
		Images:            []llm.UserImage{{Filename: "later.png", MediaType: "image/png", Data: []byte{4, 5}}},
	}); err != nil {
		t.Fatalf("insert discarded user: %v", err)
	}

	if err := ReplaceUserMessage(context.Background(), database, 1, targetID, llm.Message{
		Role:              "user",
		Content:           "edited",
		PromptAppendTexts: []string{"new append"},
	}); err != nil {
		t.Fatalf("ReplaceUserMessage() error: %v", err)
	}

	messages, err := GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("GetMessages() error: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3: %#v", len(messages), messages)
	}
	if messages[0].Content != "earlier" || messages[1].Content != "earlier answer" {
		t.Errorf("earlier messages changed: %#v", messages[:2])
	}
	if messages[2].ID != targetID || messages[2].Content != "edited" || !slices.Equal(messages[2].PromptAppendTexts, []string{"new append"}) || !reflect.DeepEqual(messages[2].Images, []llm.UserImage{{Filename: "target.png", MediaType: "image/png", Width: 4, Height: 5, Data: []byte{1, 2, 3}}}) {
		t.Errorf("edited message = %#v, want stable ID %d, edited content, and new append", messages[2], targetID)
	}
	var position, toolCalls, promptAppends int
	if err := database.QueryRow(`
		SELECT
			(SELECT position FROM messages WHERE id = ?),
			(SELECT COUNT(*) FROM message_tool_calls),
			(SELECT COUNT(*) FROM message_prompt_appends)
	`, targetID).Scan(&position, &toolCalls, &promptAppends); err != nil {
		t.Fatalf("query edited rows: %v", err)
	}
	if position != 2 || toolCalls != 0 || promptAppends != 1 {
		t.Errorf("edited rows = position %d, tool calls %d, appends %d; want 2, 0, 1", position, toolCalls, promptAppends)
	}
	var imageRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM message_user_images`).Scan(&imageRows); err != nil || imageRows != 1 {
		t.Fatalf("image rows after replacement = %d, err=%v; want target image only", imageRows, err)
	}
	before, err := GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceUserMessage(context.Background(), database, 1, targetID, llm.Message{Role: "user", Content: "must reject", Images: []llm.UserImage{{MediaType: "image/png", Data: []byte{9}}}}); err == nil {
		t.Fatal("replacement images accepted")
	}
	after, err := GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("history changed after rejected image replacement")
	}
}

func TestReplaceUserMessageInvalidatesEarlierSnapshot(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	targetID := snapshot.Messages[0].ID
	if err := ReplaceUserMessage(context.Background(), database, 1, targetID, llm.Message{Role: "user", Content: "edited"}); err != nil {
		t.Fatalf("ReplaceUserMessage() error: %v", err)
	}

	err = AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale"}})
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("AppendCompletion() error = %v, want conversation conflict", err)
	}
}

func TestReplaceUserMessageRejectsInvalidTargets(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (id, chat_id, position, role, content) VALUES (10, 1, 0, 'assistant', 'answer');
	`); err != nil {
		t.Fatalf("insert target: %v", err)
	}

	if err := ReplaceUserMessage(context.Background(), database, 1, 99, llm.Message{Role: "user", Content: "missing"}); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("missing target error = %v, want ErrMessageNotFound", err)
	}
	if err := ReplaceUserMessage(context.Background(), database, 1, 10, llm.Message{Role: "user", Content: "invalid"}); !errors.Is(err, ErrMessageNotEditable) {
		t.Errorf("assistant target error = %v, want ErrMessageNotEditable", err)
	}
	if err := ReplaceUserMessage(context.Background(), database, 2, 10, llm.Message{Role: "user", Content: "missing chat"}); !errors.Is(err, ErrChatNotFound) {
		t.Errorf("missing chat error = %v, want ErrChatNotFound", err)
	}
	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = 10`).Scan(&content); err != nil {
		t.Fatalf("query unchanged target: %v", err)
	}
	if content != "answer" {
		t.Errorf("target content = %q, want unchanged answer", content)
	}
}

func TestUndoRedoLatestTurnPreservesStoredMessages(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 0, llm.Message{Role: "user", Content: "earlier"}); err != nil {
		t.Fatalf("insert earlier user: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 1, llm.Message{Role: "assistant", Content: "earlier answer"}); err != nil {
		t.Fatalf("insert earlier assistant: %v", err)
	}
	latestID, err := InsertMessage(context.Background(), database, 1, 2, llm.Message{
		Role:              "user",
		Content:           "latest",
		PromptAppendTexts: []string{"Research carefully."},
		Images:            []llm.UserImage{{Filename: "latest.png", MediaType: "image/png", Width: 2, Height: 3, Data: []byte{7, 8}}},
	})
	if err != nil {
		t.Fatalf("insert latest user: %v", err)
	}
	metadata, err := llm.NewResponsesProviderMetadata([]json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"reasoning-1"}`)})
	if err != nil {
		t.Fatalf("create provider metadata: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 3, llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{"key":"value"}`},
		}},
		ProviderMetadata: metadata,
	}); err != nil {
		t.Fatalf("insert assistant tool call: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 4, llm.Message{Role: "tool", Content: "result", ToolCallID: "call-1"}); err != nil {
		t.Fatalf("insert tool result: %v", err)
	}
	if _, err := InsertMessage(context.Background(), database, 1, 5, llm.Message{Role: "assistant", Content: "latest answer"}); err != nil {
		t.Fatalf("insert latest assistant: %v", err)
	}

	result, err := UndoLatestTurn(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("UndoLatestTurn() error: %v", err)
	}
	if result.Message.ID != latestID || result.Message.Content != "latest" {
		t.Errorf("undone message = %#v, want latest message %d", result.Message, latestID)
	}
	if len(result.Messages) != 2 || result.Messages[1].Content != "earlier answer" {
		t.Fatalf("active messages after undo = %#v", result.Messages)
	}
	if slices.ContainsFunc(result.Messages, func(message llm.Message) bool { return len(message.Images) > 0 }) {
		t.Fatal("undone image visible in active messages")
	}
	var hidden, toolCalls, appends, outputs int
	if err := database.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM messages WHERE chat_id = 1 AND undo_sequence IS NOT NULL),
			(SELECT COUNT(*) FROM message_tool_calls),
			(SELECT COUNT(*) FROM message_prompt_appends),
			(SELECT COUNT(*) FROM message_provider_outputs)
	`).Scan(&hidden, &toolCalls, &appends, &outputs); err != nil {
		t.Fatalf("query hidden records: %v", err)
	}
	if hidden != 4 || toolCalls != 1 || appends != 1 || outputs != 1 {
		t.Errorf("hidden records = messages %d, calls %d, appends %d, outputs %d; want 4, 1, 1, 1", hidden, toolCalls, appends, outputs)
	}
	if err := ReplaceUserMessage(context.Background(), database, 1, latestID, llm.Message{Role: "user", Content: "hidden edit"}); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("ReplaceUserMessage(hidden) error = %v, want ErrMessageNotFound", err)
	}

	messages, err := RedoLatestTurn(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("RedoLatestTurn() error: %v", err)
	}
	if len(messages) != 6 {
		t.Fatalf("message count after redo = %d, want 6: %#v", len(messages), messages)
	}
	if messages[2].ID != latestID || !slices.Equal(messages[2].PromptAppendTexts, []string{"Research carefully."}) || !reflect.DeepEqual(messages[2].Images, []llm.UserImage{{Filename: "latest.png", MediaType: "image/png", Width: 2, Height: 3, Data: []byte{7, 8}}}) {
		t.Errorf("restored user message = %#v", messages[2])
	}
	if len(messages[3].ToolCalls) != 1 || messages[3].ToolCalls[0].ID != "call-1" || len(messages[3].ProviderMetadata.ResponsesOutput()) != 1 {
		t.Errorf("restored assistant message = %#v", messages[3])
	}
	if messages[4].ToolCallID != "call-1" || messages[5].Content != "latest answer" {
		t.Errorf("restored completion tail = %#v", messages[4:])
	}
	if _, err := UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatal(err)
	}
	if err := DiscardUndoneMessages(context.Background(), database, 1); err != nil {
		t.Fatal(err)
	}
	var imageRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM message_user_images`).Scan(&imageRows); err != nil || imageRows != 0 {
		t.Fatalf("image rows after discard = %d, err=%v; want 0", imageRows, err)
	}
}

func TestUndoRedoLatestTurnUsesLIFOOrder(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	for position, message := range []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "second answer"},
		{Role: "user", Content: "third"},
		{Role: "assistant", Content: "third answer"},
	} {
		if _, err := InsertMessage(context.Background(), database, 1, position, message); err != nil {
			t.Fatalf("insert message %d: %v", position, err)
		}
	}

	if _, err := UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("undo third turn: %v", err)
	}
	secondUndo, err := UndoLatestTurn(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("undo second turn: %v", err)
	}
	if secondUndo.Message.Content != "second" || len(secondUndo.Messages) != 2 {
		t.Errorf("second undo = %#v", secondUndo)
	}
	var secondSequence, thirdSequence int
	if err := database.QueryRow(`
		SELECT
			(SELECT undo_sequence FROM messages WHERE chat_id = 1 AND position = 2),
			(SELECT undo_sequence FROM messages WHERE chat_id = 1 AND position = 4)
	`).Scan(&secondSequence, &thirdSequence); err != nil {
		t.Fatalf("query undo sequences: %v", err)
	}
	if secondSequence != 2 || thirdSequence != 1 {
		t.Errorf("undo sequences = second %d, third %d; want 2, 1", secondSequence, thirdSequence)
	}

	messages, err := RedoLatestTurn(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("redo second turn: %v", err)
	}
	if len(messages) != 4 || messages[2].Content != "second" {
		t.Errorf("first redo messages = %#v", messages)
	}
	messages, err = RedoLatestTurn(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("redo third turn: %v", err)
	}
	if len(messages) != 6 || messages[4].Content != "third" {
		t.Errorf("second redo messages = %#v", messages)
	}
}

func TestUndoRedoLatestTurnRejectsUnavailableChanges(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'assistant', 'answer');
	`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}

	if _, err := UndoLatestTurn(context.Background(), database, 1); !errors.Is(err, ErrNothingToUndo) {
		t.Errorf("UndoLatestTurn() error = %v, want ErrNothingToUndo", err)
	}
	if _, err := RedoLatestTurn(context.Background(), database, 1); !errors.Is(err, ErrNothingToRedo) {
		t.Errorf("RedoLatestTurn() error = %v, want ErrNothingToRedo", err)
	}
	if _, err := UndoLatestTurn(context.Background(), database, 2); !errors.Is(err, ErrChatNotFound) {
		t.Errorf("UndoLatestTurn(missing chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := RedoLatestTurn(context.Background(), database, 2); !errors.Is(err, ErrChatNotFound) {
		t.Errorf("RedoLatestTurn(missing chat) error = %v, want ErrChatNotFound", err)
	}
}

func TestUndoRedoLatestTurnInvalidateSnapshots(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	insertMessagesTestUser(t, database)
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get original snapshot: %v", err)
	}
	if _, err := UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("undo turn: %v", err)
	}
	if err := AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "stale after undo"}}); !errors.Is(err, ErrConversationConflict) {
		t.Errorf("AppendCompletion(after undo) error = %v, want conflict", err)
	}

	undoneSnapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get undone snapshot: %v", err)
	}
	if _, err := RedoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("redo turn: %v", err)
	}
	if err := AppendCompletion(context.Background(), database, 1, undoneSnapshot, []llm.Message{{Role: "assistant", Content: "stale after redo"}}); !errors.Is(err, ErrConversationConflict) {
		t.Errorf("AppendCompletion(after redo) error = %v, want conflict", err)
	}
}

func TestAppendCompletionDiscardsRedoHistory(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'first'),
			(1, 1, 'assistant', 'first answer'),
			(1, 2, 'user', 'second'),
			(1, 3, 'assistant', 'second answer');
	`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("undo second turn: %v", err)
	}
	snapshot, err := GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get active snapshot: %v", err)
	}
	if err := AppendCompletion(context.Background(), database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "replacement"}}); err != nil {
		t.Fatalf("AppendCompletion() error: %v", err)
	}
	var hidden int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE undo_sequence IS NOT NULL`).Scan(&hidden); err != nil {
		t.Fatalf("count hidden messages: %v", err)
	}
	if hidden != 0 {
		t.Errorf("hidden message count = %d, want 0", hidden)
	}
	if _, err := RedoLatestTurn(context.Background(), database, 1); !errors.Is(err, ErrNothingToRedo) {
		t.Errorf("RedoLatestTurn() error = %v, want ErrNothingToRedo", err)
	}
	messages, err := GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("GetMessages() error: %v", err)
	}
	if len(messages) != 3 || messages[2].Content != "replacement" {
		t.Errorf("messages after replacement = %#v", messages)
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

func insertNamedChat(t *testing.T, database *sql.DB, title string, tools, appendIDs []string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := database.QueryRowContext(ctx, `INSERT INTO chats (title) VALUES (?) RETURNING id`, title).Scan(&id); err != nil {
		t.Fatalf("insert chat %q: %v", title, err)
	}
	if err := replaceChatOptions(ctx, database, id, tools, appendIDs); err != nil {
		t.Fatalf("store options for chat %q: %v", title, err)
	}
	return id
}
