package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

const (
	defaultModelSetting       = "default_model"
	maxToolRoundsSetting      = "max_tool_rounds"
	defaultToolsSetting       = "default_tools"
	MaxConfigurableToolRounds = 100
)

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

// GetMaxToolRounds returns the stored maximum consecutive tool-call rounds or
// fallback when no valid value has been stored.
func GetMaxToolRounds(ctx context.Context, db *sql.DB, fallback int) (int, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, maxToolRoundsSetting).Scan(&value)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get max tool rounds: %w", err)
	}
	rounds, parseErr := strconv.Atoi(strings.TrimSpace(value))
	if parseErr != nil || rounds < 1 {
		return fallback, nil
	}
	return rounds, nil
}

// SetMaxToolRounds stores the maximum consecutive tool-call rounds.
func SetMaxToolRounds(ctx context.Context, db *sql.DB, rounds int) error {
	if rounds < 1 || rounds > MaxConfigurableToolRounds {
		return fmt.Errorf("set max tool rounds: rounds must be between 1 and %d", MaxConfigurableToolRounds)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value
	`, maxToolRoundsSetting, strconv.Itoa(rounds)); err != nil {
		return fmt.Errorf("set max tool rounds: %w", err)
	}
	return nil
}

// GetDefaultEnabledTools returns the tools enabled by default in new chats or
// fallback when no default has been stored.
func GetDefaultEnabledTools(ctx context.Context, db *sql.DB, fallback []string) ([]string, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, defaultToolsSetting).Scan(&value)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get default enabled tools: %w", err)
	}
	names, decodeErr := decodeToolNames(strings.TrimSpace(value))
	if decodeErr != nil {
		return fallback, nil
	}
	return names, nil
}

// SetDefaultEnabledTools stores the tools enabled by default in new chats.
func SetDefaultEnabledTools(ctx context.Context, db *sql.DB, names []string) error {
	encoded, err := encodeToolNames(names)
	if err != nil {
		return fmt.Errorf("encode default enabled tools: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value
	`, defaultToolsSetting, encoded); err != nil {
		return fmt.Errorf("set default enabled tools: %w", err)
	}
	return nil
}
