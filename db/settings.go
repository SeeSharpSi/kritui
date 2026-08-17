package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const MaxConfigurableToolRounds = 100

// settingWriter is satisfied by both *sql.DB and *sql.Tx so setting writes can
// run inside a shared transaction.
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

// SaveSettings stores all submitted settings in one transaction.
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

// GetDefaultModel returns the stored default model or fallback when unset.
func GetDefaultModel(ctx context.Context, db *sql.DB, fallback string) (string, error) {
	var model sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT default_model FROM settings WHERE id = 1`).Scan(&model); err != nil {
		return "", fmt.Errorf("get default model: %w", err)
	}
	if !model.Valid {
		return strings.TrimSpace(fallback), nil
	}
	return strings.TrimSpace(model.String), nil
}

// EnsureDefaultModel stores model only when no default has been configured yet.
func EnsureDefaultModel(ctx context.Context, db *sql.DB, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE settings SET default_model = ?
		WHERE id = 1 AND default_model IS NULL
	`, model); err != nil {
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
	if _, err := db.ExecContext(ctx, `UPDATE settings SET default_model = ? WHERE id = 1`, model); err != nil {
		return fmt.Errorf("set default model: %w", err)
	}
	return nil
}

// GetMaxToolRounds returns the stored maximum consecutive tool-call rounds or
// fallback when unset.
func GetMaxToolRounds(ctx context.Context, db *sql.DB, fallback int) (int, error) {
	var rounds sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max_tool_rounds FROM settings WHERE id = 1`).Scan(&rounds); err != nil {
		return 0, fmt.Errorf("get max tool rounds: %w", err)
	}
	if !rounds.Valid {
		return fallback, nil
	}
	return int(rounds.Int64), nil
}

// SetMaxToolRounds stores the maximum consecutive tool-call rounds.
func SetMaxToolRounds(ctx context.Context, db *sql.DB, rounds int) error {
	return setMaxToolRounds(ctx, db, rounds)
}

func setMaxToolRounds(ctx context.Context, db settingWriter, rounds int) error {
	if err := validateMaxToolRounds(rounds); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET max_tool_rounds = ? WHERE id = 1`, rounds); err != nil {
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

// GetDefaultEnabledTools returns tools enabled by default or fallback when the
// collection has never been configured.
func GetDefaultEnabledTools(ctx context.Context, db *sql.DB, fallback []string) ([]string, error) {
	var configured bool
	if err := db.QueryRowContext(ctx, `SELECT default_tools_configured FROM settings WHERE id = 1`).Scan(&configured); err != nil {
		return nil, fmt.Errorf("get default enabled tools state: %w", err)
	}
	if !configured {
		return fallback, nil
	}

	rows, err := db.QueryContext(ctx, `SELECT name FROM default_tools ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("get default enabled tools: %w", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan default enabled tool: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate default enabled tools: %w", err)
	}
	return names, nil
}

// SetDefaultEnabledTools replaces tools enabled by default for new chats.
func SetDefaultEnabledTools(ctx context.Context, db *sql.DB, names []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set default enabled tools: %w", err)
	}
	defer tx.Rollback()
	if err := setDefaultEnabledTools(ctx, tx, names); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set default enabled tools: %w", err)
	}
	return nil
}

func setDefaultEnabledTools(ctx context.Context, db settingWriter, names []string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM default_tools`); err != nil {
		return fmt.Errorf("clear default enabled tools: %w", err)
	}
	for position, name := range names {
		if _, err := db.ExecContext(ctx, `INSERT INTO default_tools (position, name) VALUES (?, ?)`, position, name); err != nil {
			return fmt.Errorf("store default enabled tool %d: %w", position, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET default_tools_configured = 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("mark default enabled tools configured: %w", err)
	}
	return nil
}
