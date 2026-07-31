package kritui_db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"seesharpsi/kritui/llm"
)

// InsertChat creates a chat and returns its database ID.
func InsertChat(ctx context.Context, db *sql.DB, title string, tools []string) (int64, error) {
	if tools == nil {
		tools = []string{}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return 0, fmt.Errorf("encode chat tools: %w", err)
	}

	result, err := db.ExecContext(ctx, `INSERT INTO chats (title, tools) VALUES (?, ?)`, title, string(encoded))
	if err != nil {
		return 0, fmt.Errorf("insert chat: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted chat ID: %w", err)
	}

	return id, nil
}

// SetChatTools replaces the enabled tool names stored for a chat.
func SetChatTools(ctx context.Context, db *sql.DB, chatID int64, tools []string) error {
	if tools == nil {
		tools = []string{}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("encode chat tools: %w", err)
	}

	result, err := db.ExecContext(ctx, `
		UPDATE chats
		SET tools = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, string(encoded), chatID)
	if err != nil {
		return fmt.Errorf("set chat tools: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set chat tools rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("set chat tools: chat %d not found", chatID)
	}
	return nil
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
	var model any
	if message.Model != "" {
		model = message.Model
	}

	result, err := db.ExecContext(ctx, `
		INSERT INTO messages (chat_id, position, role, content, model, tool_calls, tool_call_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, chatID, position, message.Role, message.Content, model, toolCalls, toolCallID)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted message ID: %w", err)
	}

	return id, nil
}
