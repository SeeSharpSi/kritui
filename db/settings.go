package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const defaultModelSetting = "default_model"

// GetDefaultModel returns the stored default model or fallback when no default has been stored.
func GetDefaultModel(ctx context.Context, db *sql.DB, fallback string) (string, error) {
	var model string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, defaultModelSetting).Scan(&model)
	if err == sql.ErrNoRows {
		return strings.TrimSpace(fallback), nil
	}
	if err != nil {
		return "", fmt.Errorf("get default model: %w", err)
	}
	return strings.TrimSpace(model), nil
}

// EnsureDefaultModel stores model only when no default has been configured yet.
func EnsureDefaultModel(ctx context.Context, db *sql.DB, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO NOTHING
	`, defaultModelSetting, model); err != nil {
		return fmt.Errorf("ensure default model: %w", err)
	}
	return nil
}

// SetDefaultModel replaces the model selected by default for new chats.
func SetDefaultModel(ctx context.Context, db *sql.DB, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("set default model: model is required")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value
	`, defaultModelSetting, model); err != nil {
		return fmt.Errorf("set default model: %w", err)
	}
	return nil
}
