package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ToolCallElision identifies one stored tool call by its owning message and
// position. Provider call IDs are not unique across messages, so they are not
// used as the update key.
type ToolCallElision struct {
	MessageID int64
	Position  int
	Elided    bool
}

// SetToolCallElisions updates display-only elision metadata for active
// assistant tool calls in a chat owned by the explicit user in ctx. Every
// update is matched by message ID and call position. Missing, hidden, foreign,
// or non-assistant targets are rejected without changing any rows.
func SetToolCallElisions(ctx context.Context, db databaseExecutor, chatID int64, updates []ToolCallElision) error {
	if len(updates) == 0 {
		return nil
	}
	if _, err := requireUserID(ctx); err != nil {
		return fmt.Errorf("set tool call elisions: %w", err)
	}
	return executeAtomically(ctx, db, func(executor databaseExecutor) error {
		if err := requireChatOwned(ctx, executor, chatID); err != nil {
			return err
		}
		for _, update := range updates {
			var stored int
			err := executor.QueryRowContext(ctx, `
				SELECT calls.result_elided
				FROM message_tool_calls AS calls
				JOIN messages ON messages.id = calls.message_id
				WHERE messages.chat_id = ?
					AND messages.id = ?
					AND messages.role = 'assistant'
					AND messages.undo_sequence IS NULL
					AND calls.position = ?
			`, chatID, update.MessageID, update.Position).Scan(&stored)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMessageNotFound
			}
			if err != nil {
				return fmt.Errorf("inspect tool call %d for message %d: %w", update.Position, update.MessageID, err)
			}

			want := 0
			if update.Elided {
				want = 1
			}
			if stored == want {
				continue
			}
			result, err := executor.ExecContext(ctx, `
				UPDATE message_tool_calls
				SET result_elided = ?
				WHERE message_id = ? AND position = ?
			`, want, update.MessageID, update.Position)
			if err != nil {
				return fmt.Errorf("update tool call %d for message %d: %w", update.Position, update.MessageID, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("update tool call %d for message %d rows affected: %w", update.Position, update.MessageID, err)
			}
			if affected == 0 {
				return ErrMessageNotFound
			}
		}
		return nil
	})
}
