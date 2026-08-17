package kritui_db

import (
	"context"
	"database/sql"
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
	if err := replaceChatOptions(ctx, tx, id, tools, appendIDs); err != nil {
		return 0, err
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
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin insert chat: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO chats (title) VALUES (?)`, title)
	if err != nil {
		return 0, fmt.Errorf("insert chat: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted chat ID: %w", err)
	}
	if err := replaceChatOptions(ctx, tx, id, tools, appendIDs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit insert chat: %w", err)
	}
	return id, nil
}

// SetChatTools replaces enabled tool names stored for a chat.
func SetChatTools(ctx context.Context, db *sql.DB, chatID int64, tools []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set chat tools: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE chats
		SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, chatID)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_tools WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("clear chat tools: %w", err)
	}
	if err := insertChatTools(ctx, tx, chatID, tools); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set chat tools: %w", err)
	}
	return nil
}

// UpsertChat inserts or updates a chat and atomically replaces its options.
// Existing non-empty titles are preserved.
func UpsertChat(ctx context.Context, db databaseExecutor, chatID int64, title string, tools, appendIDs []string) error {
	return executeAtomically(ctx, db, func(executor databaseExecutor) error {
		if _, err := executor.ExecContext(ctx, `
			INSERT INTO chats (id, title) VALUES (?, ?)
			ON CONFLICT (id) DO UPDATE SET
				title = CASE WHEN chats.title = '' THEN excluded.title ELSE chats.title END
		`, chatID, title); err != nil {
			return fmt.Errorf("upsert chat: %w", err)
		}
		return replaceChatOptions(ctx, executor, chatID, tools, appendIDs)
	})
}

func replaceChatOptions(ctx context.Context, db databaseExecutor, chatID int64, tools, appendIDs []string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM chat_tools WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("clear chat tools: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM chat_prompt_appends WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("clear chat prompt appends: %w", err)
	}
	if err := insertChatTools(ctx, db, chatID, tools); err != nil {
		return err
	}
	for position, id := range appendIDs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO chat_prompt_appends (chat_id, position, prompt_append_id)
			VALUES (?, ?, ?)
		`, chatID, position, id); err != nil {
			return fmt.Errorf("store prompt append %d for chat %d: %w", position, chatID, err)
		}
	}
	return nil
}

func insertChatTools(ctx context.Context, db databaseExecutor, chatID int64, tools []string) error {
	for position, name := range tools {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO chat_tools (chat_id, position, name) VALUES (?, ?, ?)
		`, chatID, position, name); err != nil {
			return fmt.Errorf("store tool %d for chat %d: %w", position, chatID, err)
		}
	}
	return nil
}

// InsertMessage adds a message at its position in a chat and returns its database ID.
func InsertMessage(ctx context.Context, db databaseExecutor, chatID int64, position int, message llm.Message) (int64, error) {
	var id int64
	err := executeAtomically(ctx, db, func(executor databaseExecutor) error {
		insertedID, err := insertMessage(ctx, executor, chatID, position, message)
		id = insertedID
		return err
	})
	return id, err
}

func insertMessage(ctx context.Context, db databaseExecutor, chatID int64, position int, message llm.Message) (int64, error) {
	if len(message.ToolCalls) > 0 && message.Role != "assistant" {
		return 0, fmt.Errorf("store message tool calls: only assistant messages may contain tool calls")
	}
	if len(message.PromptAppendTexts) > 0 && message.Role != "user" {
		return 0, fmt.Errorf("store prompt append texts: only user messages may contain prompt append texts")
	}
	if !message.ProviderMetadata.IsZero() && message.Role != "assistant" {
		return 0, fmt.Errorf("store provider metadata: only assistant messages may contain provider metadata")
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

	result, err := db.ExecContext(ctx, `
		INSERT INTO messages (chat_id, position, role, content, model, total_tokens, cost, tool_call_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, chatID, position, message.Role, message.Content, model, totalTokens, cost, toolCallID)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get inserted message ID: %w", err)
	}

	for callPosition, call := range message.ToolCalls {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_tool_calls
				(message_id, message_role, position, call_id, call_type, function_name, arguments)
			VALUES (?, 'assistant', ?, ?, ?, ?, ?)
		`, id, callPosition, call.ID, call.Type, call.Function.Name, call.Function.Arguments); err != nil {
			return 0, fmt.Errorf("store tool call %d for message %d: %w", callPosition, id, err)
		}
	}
	for appendPosition, text := range message.PromptAppendTexts {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_prompt_appends (message_id, message_role, position, text)
			VALUES (?, 'user', ?, ?)
		`, id, appendPosition, text); err != nil {
			return 0, fmt.Errorf("store prompt append %d for message %d: %w", appendPosition, id, err)
		}
	}
	for outputPosition, output := range message.ProviderMetadata.ResponsesOutput() {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_provider_outputs (message_id, message_role, position, payload)
			VALUES (?, 'assistant', ?, ?)
		`, id, outputPosition, string(output)); err != nil {
			return 0, fmt.Errorf("store provider output %d for message %d: %w", outputPosition, id, err)
		}
	}
	return id, nil
}

func executeAtomically(ctx context.Context, db databaseExecutor, operation func(databaseExecutor) error) error {
	if _, ok := db.(*sql.Tx); ok {
		return operation(db)
	}
	database, ok := db.(*sql.DB)
	if !ok {
		return fmt.Errorf("unsupported database executor %T", db)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin database transaction: %w", err)
	}
	defer tx.Rollback()
	if err := operation(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit database transaction: %w", err)
	}
	return nil
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
