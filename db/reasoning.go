package kritui_db

import (
	"context"
	"database/sql"
	"fmt"

	"seesharpsi/kritui/llm"
)

// GetDefaultReasoningEffort returns the account's default thinking level.
// An empty value leaves the provider's default unchanged.
func GetDefaultReasoningEffort(ctx context.Context, db databaseExecutor) (string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return "", err
	}
	var effort string
	if err := db.QueryRowContext(ctx, `SELECT default_reasoning_effort FROM settings WHERE id = ?`, userID).Scan(&effort); err != nil {
		return "", fmt.Errorf("get default thinking level: %w", err)
	}
	return effort, nil
}

// GetChatReasoningEffort resolves a nullable chat override against the account
// default. An explicit empty override means provider Default, not inheritance.
func GetChatReasoningEffort(ctx context.Context, db databaseExecutor, chatID int64) (string, error) {
	if err := requireChatOwned(ctx, db, chatID); err != nil {
		return "", err
	}
	var effort sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT reasoning_effort FROM chats WHERE id = ? AND user_id = ?`, chatID, UserID(ctx)).Scan(&effort); err != nil {
		return "", fmt.Errorf("get chat thinking level: %w", err)
	}
	if effort.Valid {
		return effort.String, nil
	}
	return GetDefaultReasoningEffort(ctx, db)
}

// SetChatReasoningEffort saves an explicit override without changing recency.
func SetChatReasoningEffort(ctx context.Context, db databaseExecutor, chatID int64, effort string) error {
	if err := llm.ValidateReasoningEffort(effort); err != nil {
		return err
	}
	if err := requireChatOwned(ctx, db, chatID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE chats SET reasoning_effort = ? WHERE id = ? AND user_id = ?`, effort, chatID, UserID(ctx))
	return err
}
