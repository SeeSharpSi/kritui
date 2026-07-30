package kritui_db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"seesharpsi/kritui/llm"
)

// InsertChat creates a chat and returns its database ID.
func InsertChat(ctx context.Context, db *sql.DB, title string) (int64, error) {
	result, err := db.ExecContext(ctx, `INSERT INTO chats (title) VALUES (?)`, title)
	if err != nil {
		return 0, fmt.Errorf("insert chat: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted chat ID: %w", err)
	}

	return id, nil
}

// InsertMessage adds a message at its position in a chat and returns its database ID.
func InsertMessage(ctx context.Context, db *sql.DB, chatID int64, position int, message llm.Message) (int64, error) {
	var toolCalls any
	if len(message.ToolCalls) > 0 {
		encoded, err := json.Marshal(message.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("encode message tool calls: %w", err)
		}
		toolCalls = string(encoded)
	}

	var toolCallID any
	if message.ToolCallID != "" {
		toolCallID = message.ToolCallID
	}

	result, err := db.ExecContext(ctx, `
		INSERT INTO messages (chat_id, position, role, content, tool_calls, tool_call_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, chatID, position, message.Role, message.Content, toolCalls, toolCallID)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted message ID: %w", err)
	}

	return id, nil
}
