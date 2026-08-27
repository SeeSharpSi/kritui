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
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var (
	// ErrConversationConflict means stored history changed after model work began.
	ErrConversationConflict = errors.New("conversation changed")
	// ErrChatNotFound means completion target was deleted before persistence.
	ErrChatNotFound = errors.New("chat not found")
	// ErrMessageNotFound means an edit target does not belong to the chat.
	ErrMessageNotFound = errors.New("message not found")
	// ErrMessageNotEditable means an edit target is not a user message.
	ErrMessageNotEditable = errors.New("message is not editable")
	// ErrNothingToUndo means a chat has no active user turn.
	ErrNothingToUndo = errors.New("nothing to undo")
	// ErrNothingToRedo means a chat has no hidden user turn.
	ErrNothingToRedo = errors.New("nothing to redo")
)

// UndoResult contains the newly active history and user message removed by an
// undo operation.
type UndoResult struct {
	Messages []llm.Message
	Message  llm.Message
}

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
	if len(message.Images) > 0 && message.Role != "user" {
		return 0, fmt.Errorf("store images: only user messages may contain images")
	}
	for imagePosition, image := range message.Images {
		if image.MediaType == "" || len(image.Data) == 0 || image.Width < 0 || image.Height < 0 {
			return 0, fmt.Errorf("store image %d: invalid media type, data, or dimensions", imagePosition)
		}
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
	for imagePosition, image := range message.Images {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_user_images
				(message_id, message_role, position, filename, media_type, width, height, data)
			VALUES (?, 'user', ?, ?, ?, ?, ?, ?)
		`, id, imagePosition, image.Filename, image.MediaType, image.Width, image.Height, image.Data); err != nil {
			return 0, fmt.Errorf("store image %d for message %d: %w", imagePosition, id, err)
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

// ReplaceUserMessage atomically replaces one user message and deletes every
// later message in the chat. The target keeps its database ID and position.
func ReplaceUserMessage(ctx context.Context, db databaseExecutor, chatID, messageID int64, message llm.Message) error {
	if message.Role != "user" {
		return ErrMessageNotEditable
	}
	if message.Model != "" || message.TotalTokens != nil || message.Cost != nil || len(message.ToolCalls) > 0 || message.ToolCallID != "" || !message.ProviderMetadata.IsZero() {
		return fmt.Errorf("replace user message: invalid user message metadata")
	}
	if len(message.Images) > 0 {
		return fmt.Errorf("replace user message: image replacement is unsupported")
	}

	return executeAtomically(ctx, db, func(executor databaseExecutor) error {
		// First write reserves the SQLite writer lock before locating the target.
		result, err := executor.ExecContext(ctx, `UPDATE chats SET id = id WHERE id = ?`, chatID)
		if err != nil {
			return fmt.Errorf("reserve chat for message edit: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reserve edited chat rows affected: %w", err)
		}
		if affected == 0 {
			return ErrChatNotFound
		}

		var position int
		var role string
		if err := executor.QueryRowContext(ctx, `
			SELECT position, role
			FROM messages
			WHERE id = ? AND chat_id = ? AND undo_sequence IS NULL
		`, messageID, chatID).Scan(&position, &role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMessageNotFound
			}
			return fmt.Errorf("get edited message: %w", err)
		}
		if role != "user" {
			return ErrMessageNotEditable
		}

		if _, err := executor.ExecContext(ctx, `DELETE FROM messages WHERE chat_id = ? AND position > ?`, chatID, position); err != nil {
			return fmt.Errorf("truncate messages after edit: %w", err)
		}
		if _, err := executor.ExecContext(ctx, `UPDATE messages SET content = ? WHERE id = ?`, message.Content, messageID); err != nil {
			return fmt.Errorf("replace edited message: %w", err)
		}
		if _, err := executor.ExecContext(ctx, `DELETE FROM message_prompt_appends WHERE message_id = ?`, messageID); err != nil {
			return fmt.Errorf("clear edited message prompt appends: %w", err)
		}
		for position, text := range message.PromptAppendTexts {
			if _, err := executor.ExecContext(ctx, `
				INSERT INTO message_prompt_appends (message_id, message_role, position, text)
				VALUES (?, 'user', ?, ?)
			`, messageID, position, text); err != nil {
				return fmt.Errorf("store edited message prompt append %d: %w", position, err)
			}
		}
		return nil
	})
}

// UndoLatestTurn hides the latest active user message and every active message
// after it. Repeated calls create ordered groups for LIFO redo.
func UndoLatestTurn(ctx context.Context, db *sql.DB, chatID int64) (UndoResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return UndoResult{}, fmt.Errorf("begin undo transaction: %w", err)
	}
	defer tx.Rollback()

	if err := reserveChat(ctx, tx, chatID); err != nil {
		return UndoResult{}, err
	}

	var (
		position int
		message  = llm.Message{Role: "user"}
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, position, content
		FROM messages
		WHERE chat_id = ? AND role = 'user' AND undo_sequence IS NULL
		ORDER BY position DESC
		LIMIT 1
	`, chatID).Scan(&message.ID, &position, &message.Content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UndoResult{}, ErrNothingToUndo
		}
		return UndoResult{}, fmt.Errorf("get message to undo: %w", err)
	}

	var sequence int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(undo_sequence), 0) + 1
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&sequence); err != nil {
		return UndoResult{}, fmt.Errorf("get undo sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET undo_sequence = ?
		WHERE chat_id = ? AND position >= ? AND undo_sequence IS NULL
	`, sequence, chatID, position); err != nil {
		return UndoResult{}, fmt.Errorf("undo latest turn: %w", err)
	}

	snapshot, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return UndoResult{}, fmt.Errorf("reload messages after undo: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UndoResult{}, fmt.Errorf("commit undo: %w", err)
	}
	return UndoResult{Messages: snapshot.Messages, Message: message}, nil
}

// RedoLatestTurn restores the most recently hidden undo group.
func RedoLatestTurn(ctx context.Context, db *sql.DB, chatID int64) ([]llm.Message, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin redo transaction: %w", err)
	}
	defer tx.Rollback()

	if err := reserveChat(ctx, tx, chatID); err != nil {
		return nil, err
	}

	var sequence sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(undo_sequence)
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&sequence); err != nil {
		return nil, fmt.Errorf("get redo sequence: %w", err)
	}
	if !sequence.Valid {
		return nil, ErrNothingToRedo
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET undo_sequence = NULL
		WHERE chat_id = ? AND undo_sequence = ?
	`, chatID, sequence.Int64); err != nil {
		return nil, fmt.Errorf("redo latest turn: %w", err)
	}

	snapshot, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return nil, fmt.Errorf("reload messages after redo: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit redo: %w", err)
	}
	return snapshot.Messages, nil
}

// DiscardUndoneMessages permanently removes a chat's hidden redo history.
func DiscardUndoneMessages(ctx context.Context, db databaseExecutor, chatID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE chat_id = ? AND undo_sequence IS NOT NULL`, chatID); err != nil {
		return fmt.Errorf("discard undone messages: %w", err)
	}
	return nil
}

func reserveChat(ctx context.Context, db databaseExecutor, chatID int64) error {
	result, err := db.ExecContext(ctx, `UPDATE chats SET id = id WHERE id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("reserve chat history: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reserve chat history rows affected: %w", err)
	}
	if affected == 0 {
		return ErrChatNotFound
	}
	return nil
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
	if err := DiscardUndoneMessages(ctx, tx, chatID); err != nil {
		return err
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
