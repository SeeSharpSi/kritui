package kritui_db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"seesharpsi/kritui/llm"
)

type databaseExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// AllocateChat reserves a unique chat ID and removes abandoned empty chats.
func AllocateChat(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin chat allocation: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO chats DEFAULT VALUES`)
	if err != nil {
		return 0, fmt.Errorf("allocate chat: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get allocated chat ID: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM chats
		WHERE id <> ?
			AND created_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-24 hours')
			AND NOT EXISTS (
				SELECT 1
				FROM messages
				WHERE messages.chat_id = chats.id
			)
	`, id); err != nil {
		return 0, fmt.Errorf("remove abandoned chats: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit chat allocation: %w", err)
	}
	return id, nil
}

// InsertChat creates a chat and returns its database ID.
func InsertChat(ctx context.Context, db *sql.DB, title string, tools []string) (int64, error) {
	encoded, err := encodeToolNames(tools)
	if err != nil {
		return 0, fmt.Errorf("encode chat tools: %w", err)
	}

	result, err := db.ExecContext(ctx, `INSERT INTO chats (title, tools) VALUES (?, ?)`, title, encoded)
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
	encoded, err := encodeToolNames(tools)
	if err != nil {
		return fmt.Errorf("encode chat tools: %w", err)
	}

	result, err := db.ExecContext(ctx, `
		UPDATE chats
		SET tools = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, encoded, chatID)
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
func InsertMessage(ctx context.Context, db databaseExecutor, chatID int64, position int, message llm.Message) (int64, error) {
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
	var totalTokens any
	if message.TotalTokens != nil {
		totalTokens = *message.TotalTokens
	}
	var cost any
	if message.Cost != nil {
		cost = *message.Cost
	}
	var providerMetadata any
	if !message.ProviderMetadata.IsZero() {
		if message.Role != "assistant" {
			return 0, fmt.Errorf("encode provider metadata: only assistant messages may contain provider metadata")
		}
		encoded, err := json.Marshal(message.ProviderMetadata)
		if err != nil {
			return 0, fmt.Errorf("encode provider metadata: %w", err)
		}
		providerMetadata = string(encoded)
	}

	result, err := db.ExecContext(ctx, `
		INSERT INTO messages (chat_id, position, role, content, model, total_tokens, cost, tool_calls, tool_call_id, provider_metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, chatID, position, message.Role, message.Content, model, totalTokens, cost, toolCalls, toolCallID, providerMetadata)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted message ID: %w", err)
	}

	return id, nil
}
