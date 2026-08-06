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

// settingWriter is satisfied by both *sql.DB and *sql.Tx so setting
// upserts can run inside a shared transaction.
type settingWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// SettingsUpdate describes the desired settings for one atomic save.
// PromptAppends is used only when non-nil; a nil value leaves configured
// prompt appends untouched.
type SettingsUpdate struct {
	Model         string
	MaxToolRounds int
	DefaultTools  []string
	PromptAppends []PromptAppend
}

// SaveSettings stores the default model, max tool rounds, default tools, and,
// when PromptAppends is non-nil, the prompt append definitions together in one
// transaction. Prompt-append upsert and chat-selection pruning share that same
// transaction, so any failure rolls back every value changed by the request.
func SaveSettings(ctx context.Context, db *sql.DB, update SettingsUpdate) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set settings: %w", err)
	}
	defer tx.Rollback()

	if err := setDefaultModel(ctx, tx, update.Model); err != nil {
		return err
	}
	if err := setMaxToolRounds(ctx, tx, update.MaxToolRounds); err != nil {
		return err
	}
	if err := setDefaultEnabledTools(ctx, tx, update.DefaultTools); err != nil {
		return err
	}
	if update.PromptAppends != nil {
		if err := setPromptAppends(ctx, tx, update.PromptAppends); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set settings: %w", err)
	}
	return nil
}

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
	return setDefaultModel(ctx, db, model)
}

func setDefaultModel(ctx context.Context, db settingWriter, model string) error {
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
	return setMaxToolRounds(ctx, db, rounds)
}

func setMaxToolRounds(ctx context.Context, db settingWriter, rounds int) error {
	if err := validateMaxToolRounds(rounds); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value
	`, maxToolRoundsSetting, strconv.Itoa(rounds)); err != nil {
		return fmt.Errorf("set max tool rounds: %w", err)
	}
	return nil
}

func validateMaxToolRounds(rounds int) error {
	if rounds < 1 || rounds > MaxConfigurableToolRounds {
		return fmt.Errorf("set max tool rounds: rounds must be between 1 and %d", MaxConfigurableToolRounds)
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
	return setDefaultEnabledTools(ctx, db, names)
}

func setDefaultEnabledTools(ctx context.Context, db settingWriter, names []string) error {
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
