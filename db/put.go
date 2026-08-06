package kritui_db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"seesharpsi/kritui/llm"
)

type databaseExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

var (
	// ErrConversationConflict means stored history changed after model work began.
	ErrConversationConflict = errors.New("conversation changed")
	// ErrChatNotFound means completion target was deleted before persistence.
	ErrChatNotFound = errors.New("chat not found")
)

// AllocateChat reserves a unique chat ID, stores options enabled by default
// for new chats, and removes abandoned empty chats.
func AllocateChat(ctx context.Context, db *sql.DB, tools, appendIDs []string) (int64, error) {
	encoded, err := encodeToolNames(tools)
	if err != nil {
		return 0, fmt.Errorf("encode chat tools: %w", err)
	}
	encodedAppendIDs, err := encodePromptAppendIDs(appendIDs)
	if err != nil {
		return 0, fmt.Errorf("encode chat prompt append IDs: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin chat allocation: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO chats (tools, appends) VALUES (?, ?)`, encoded, encodedAppendIDs)
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
func InsertChat(ctx context.Context, db *sql.DB, title string, tools, appendIDs []string) (int64, error) {
	encoded, err := encodeToolNames(tools)
	if err != nil {
		return 0, fmt.Errorf("encode chat tools: %w", err)
	}
	encodedAppendIDs, err := encodePromptAppendIDs(appendIDs)
	if err != nil {
		return 0, fmt.Errorf("encode chat prompt append IDs: %w", err)
	}

	result, err := db.ExecContext(ctx, `INSERT INTO chats (title, tools, appends) VALUES (?, ?, ?)`, title, encoded, encodedAppendIDs)
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

// SetChatPromptAppendIDs replaces the selected prompt append IDs stored for a chat.
func SetChatPromptAppendIDs(ctx context.Context, db *sql.DB, chatID int64, appendIDs []string) error {
	encoded, err := encodePromptAppendIDs(appendIDs)
	if err != nil {
		return fmt.Errorf("encode chat prompt append IDs: %w", err)
	}

	result, err := db.ExecContext(ctx, `
		UPDATE chats
		SET appends = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, encoded, chatID)
	if err != nil {
		return fmt.Errorf("set chat prompt append IDs: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set chat prompt append IDs rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("set chat prompt append IDs: chat %d not found", chatID)
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
	var promptAppendTexts any
	if len(message.PromptAppendTexts) > 0 {
		if message.Role != "user" {
			return 0, fmt.Errorf("encode prompt append texts: only user messages may contain prompt append texts")
		}
		encoded, err := json.Marshal(message.PromptAppendTexts)
		if err != nil {
			return 0, fmt.Errorf("encode prompt append texts: %w", err)
		}
		promptAppendTexts = string(encoded)
	}

	result, err := db.ExecContext(ctx, `
		INSERT INTO messages (chat_id, position, role, content, model, total_tokens, cost, tool_calls, tool_call_id, provider_metadata, prompt_appends)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, chatID, position, message.Role, message.Content, model, totalTokens, cost, toolCalls, toolCallID, providerMetadata, promptAppendTexts)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted message ID: %w", err)
	}

	return id, nil
}

// AppendCompletion atomically appends generated messages if stored history
// still matches the snapshot used to generate them.
func AppendCompletion(ctx context.Context, db *sql.DB, chatID int64, expected MessageSnapshot, messages []llm.Message) error {
	if len(messages) == 0 {
		return errors.New("append completion: at least one message is required")
	}
	if expected.chatID != chatID {
		return ErrConversationConflict
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin completion transaction: %w", err)
	}
	defer tx.Rollback()

	// First write reserves the SQLite writer lock before history validation.
	result, err := tx.ExecContext(ctx, `UPDATE chats SET id = id WHERE id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("reserve chat for completion: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reserve chat rows affected: %w", err)
	}
	if affected == 0 {
		return ErrChatNotFound
	}

	current, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return fmt.Errorf("reload messages for completion: %w", err)
	}
	if current.version != expected.version {
		return ErrConversationConflict
	}

	nextPosition := current.version.lastPosition + 1
	for index, message := range messages {
		if _, err := InsertMessage(ctx, tx, chatID, nextPosition+index, message); err != nil {
			return fmt.Errorf("append completion message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}
