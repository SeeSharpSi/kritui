package kritui_db

import (
	"context"
	"errors"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestToolCallResultElidedRoundTrips(t *testing.T) {
	ctx := WithUser(context.Background(), LegacyOwnerID)
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	messageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{ID: "call-full", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{"q":"full"}`}, ResultElided: true},
			{ID: "call-elided", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{"q":"elided"}`}},
		},
	})
	if err != nil {
		t.Fatalf("insert assistant tool calls: %v", err)
	}

	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}
	calls := snapshot.Messages[0].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(calls))
	}
	if !calls[0].ResultElided {
		t.Error("first tool call ResultElided = false, want true")
	}
	if calls[1].ResultElided {
		t.Error("second tool call ResultElided = true, want false")
	}

	if err := SetToolCallElisions(ctx, database, 1, []ToolCallElision{
		{MessageID: messageID, Position: 0, Elided: false},
		{MessageID: messageID, Position: 1, Elided: true},
	}); err != nil {
		t.Fatalf("SetToolCallElisions() error: %v", err)
	}

	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("get updated message snapshot: %v", err)
	}
	calls = snapshot.Messages[0].ToolCalls
	if calls[0].ResultElided {
		t.Error("first tool call ResultElided after clear = true, want false")
	}
	if !calls[1].ResultElided {
		t.Error("second tool call ResultElided after set = false, want true")
	}
}

func TestSetToolCallElisionsUpdatesDuplicateProviderIDsSeparately(t *testing.T) {
	ctx := WithUser(context.Background(), LegacyOwnerID)
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	call := llm.ToolCall{ID: "repeated-call-id", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}}
	firstID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatalf("insert first assistant: %v", err)
	}
	secondID, err := InsertMessage(ctx, database, 1, 1, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatalf("insert second assistant: %v", err)
	}

	if err := SetToolCallElisions(ctx, database, 1, []ToolCallElision{{MessageID: secondID, Position: 0, Elided: true}}); err != nil {
		t.Fatalf("SetToolCallElisions() error: %v", err)
	}

	var first, second int
	if err := database.QueryRow(`SELECT result_elided FROM message_tool_calls WHERE message_id = ?`, firstID).Scan(&first); err != nil {
		t.Fatalf("query first flag: %v", err)
	}
	if err := database.QueryRow(`SELECT result_elided FROM message_tool_calls WHERE message_id = ?`, secondID).Scan(&second); err != nil {
		t.Fatalf("query second flag: %v", err)
	}
	if first != 0 || second != 1 {
		t.Errorf("flags = %d, %d; want 0, 1", first, second)
	}
}

func TestSetToolCallElisionsRejectsInvalidTargetsAtomically(t *testing.T) {
	ctx := WithUser(context.Background(), LegacyOwnerID)
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO users (id, username) VALUES (2, 'bob');
		INSERT INTO chats (id, user_id) VALUES (2, 2);
	`); err != nil {
		t.Fatalf("seed chats: %v", err)
	}
	call := llm.ToolCall{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}}
	messageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatalf("insert assistant: %v", err)
	}
	hiddenID, err := InsertMessage(ctx, database, 1, 1, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatalf("insert hidden assistant: %v", err)
	}
	if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = ?`, hiddenID); err != nil {
		t.Fatalf("hide assistant: %v", err)
	}
	foreignID, err := InsertMessage(WithUser(context.Background(), 2), database, 2, 0, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatalf("insert foreign assistant: %v", err)
	}

	tests := []struct {
		name    string
		ctx     context.Context
		chatID  int64
		updates []ToolCallElision
		want    error
	}{
		{
			name:    "missing call position",
			ctx:     ctx,
			chatID:  1,
			updates: []ToolCallElision{{MessageID: messageID, Position: 3, Elided: true}},
			want:    ErrMessageNotFound,
		},
		{
			name:    "missing message",
			ctx:     ctx,
			chatID:  1,
			updates: []ToolCallElision{{MessageID: 999, Position: 0, Elided: true}},
			want:    ErrMessageNotFound,
		},
		{
			name:    "hidden message",
			ctx:     ctx,
			chatID:  1,
			updates: []ToolCallElision{{MessageID: hiddenID, Position: 0, Elided: true}},
			want:    ErrMessageNotFound,
		},
		{
			name:    "foreign chat",
			ctx:     ctx,
			chatID:  2,
			updates: []ToolCallElision{{MessageID: foreignID, Position: 0, Elided: true}},
			want:    ErrChatNotFound,
		},
		{
			name:    "missing chat",
			ctx:     ctx,
			chatID:  99,
			updates: []ToolCallElision{{MessageID: messageID, Position: 0, Elided: true}},
			want:    ErrChatNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := SetToolCallElisions(test.ctx, database, test.chatID, test.updates); !errors.Is(err, test.want) {
				t.Fatalf("SetToolCallElisions() error = %v, want %v", err, test.want)
			}
		})
	}

	updates := []ToolCallElision{
		{MessageID: messageID, Position: 0, Elided: true},
		{MessageID: hiddenID, Position: 0, Elided: true},
	}
	if err := SetToolCallElisions(ctx, database, 1, updates); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("SetToolCallElisions() with invalid second update error = %v, want ErrMessageNotFound", err)
	}
	var flag int
	if err := database.QueryRow(`SELECT result_elided FROM message_tool_calls WHERE message_id = ?`, messageID).Scan(&flag); err != nil {
		t.Fatalf("query rolled back flag: %v", err)
	}
	if flag != 0 {
		t.Errorf("flag after atomic rejection = %d, want 0", flag)
	}
}

func TestSetToolCallElisionsDoesNotChangeSnapshotDigestOrChatRecency(t *testing.T) {
	ctx := WithUser(context.Background(), LegacyOwnerID)
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	messageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}}},
	})
	if err != nil {
		t.Fatalf("insert assistant: %v", err)
	}
	if _, err := database.Exec(`UPDATE chats SET updated_at = '2024-01-02T03:04:05.000Z' WHERE id = 1`); err != nil {
		t.Fatalf("fix chat timestamp: %v", err)
	}
	before, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("get message snapshot: %v", err)
	}

	if err := SetToolCallElisions(ctx, database, 1, []ToolCallElision{{MessageID: messageID, Position: 0, Elided: true}}); err != nil {
		t.Fatalf("SetToolCallElisions() error: %v", err)
	}

	after, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("get updated message snapshot: %v", err)
	}
	if before.version != after.version {
		t.Errorf("snapshot version changed after elision flag: before %#v, after %#v", before.version, after.version)
	}
	if !after.Messages[0].ToolCalls[0].ResultElided {
		t.Fatal("updated snapshot did not reload the elision flag")
	}

	var updatedAt string
	if err := database.QueryRow(`SELECT updated_at FROM chats WHERE id = 1`).Scan(&updatedAt); err != nil {
		t.Fatalf("query chat recency: %v", err)
	}
	if updatedAt != "2024-01-02T03:04:05.000Z" {
		t.Errorf("chat updated_at = %q, want unchanged fixed timestamp", updatedAt)
	}

	if err := AppendCompletion(ctx, database, 1, before, []llm.Message{{Role: "assistant", Content: "continued"}}); err != nil {
		t.Fatalf("AppendCompletion() after elision flag change error: %v", err)
	}
}

func TestToolCallElidedFlagCascadesWithMessage(t *testing.T) {
	ctx := WithUser(context.Background(), LegacyOwnerID)
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 0, llm.Message{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}, ResultElided: true}},
	}); err != nil {
		t.Fatalf("insert assistant: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM chats WHERE id = 1`); err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM message_tool_calls`).Scan(&count); err != nil {
		t.Fatalf("count tool calls: %v", err)
	}
	if count != 0 {
		t.Errorf("tool call rows after chat delete = %d, want 0", count)
	}
}
