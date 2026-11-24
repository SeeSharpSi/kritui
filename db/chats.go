package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const maxChatModelBytes = 512

// SetChatModelSelection stores the provider and model selected for the next
// turn of an owned chat. Provider 0 and the synthetic environment provider are
// stored as NULL; a positive provider must belong to the current account.
func SetChatModelSelection(ctx context.Context, db databaseExecutor, chatID, providerID int64, model string) error {
	userID, err := requireUserID(ctx)
	if err != nil {
		return fmt.Errorf("set chat model selection: %w", err)
	}
	model = strings.TrimSpace(model)
	if len([]byte(model)) > maxChatModelBytes {
		return fmt.Errorf("set chat model selection: model must not exceed %d bytes", maxChatModelBytes)
	}
	var modelValue any
	if model != "" {
		modelValue = model
	}
	providerValue, err := chatProviderValue(ctx, db, userID, providerID)
	if err != nil {
		return fmt.Errorf("set chat model selection: %w", err)
	}
	result, err := db.ExecContext(ctx, `
		UPDATE chats
		SET model = ?, provider_id = ?
		WHERE id = ? AND user_id = ?
	`, modelValue, providerValue, chatID, userID)
	if err != nil {
		return fmt.Errorf("set chat model selection: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set chat model selection rows affected: %w", err)
	}
	if affected == 0 {
		return ErrChatNotFound
	}
	return nil
}

// GetChatModelSelection returns the provider and model selected for a chat.
// NULL provider and model values report zero and an empty string.
func GetChatModelSelection(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, chatID int64) (int64, string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("get chat model selection: %w", err)
	}
	var providerID sql.NullInt64
	var model sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT provider_id, model
		FROM chats
		WHERE id = ? AND user_id = ?
	`, chatID, userID).Scan(&providerID, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrChatNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("get chat model selection: %w", err)
	}
	return nullSettingInt64(providerID), nullSettingString(model), nil
}

func chatProviderValue(ctx context.Context, db databaseExecutor, userID, providerID int64) (any, error) {
	switch {
	case providerID == 0 || providerID == EnvProviderID:
		return nil, nil
	case providerID < 0:
		return nil, fmt.Errorf("provider %d cannot be stored", providerID)
	}
	var exists int
	if err := db.QueryRowContext(ctx, `
		SELECT 1 FROM providers WHERE id = ? AND user_id = ?
	`, providerID, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("provider %d is not owned by this account", providerID)
		}
		return nil, fmt.Errorf("check provider ownership: %w", err)
	}
	return providerID, nil
}
