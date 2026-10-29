package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	kritui_db "seesharpsi/kritui/db"
)

// migrateUserAccounts upgrades a version 23 database to per-user ownership.
// It creates users and sessions, seeds the disabled legacy owner, turns the
// legacy settings row into owner 1's settings, and adds user ownership to
// chats and every user-scoped definition table. Existing rows are preserved
// byte for byte, including secrets, selections, images, undo groups, provider
// output, IDs, and timestamps. The function is idempotent so tests that start
// from the newest schema and replay older migrations stay safe.
func migrateUserAccounts(ctx context.Context, tx *sql.Tx) error {
	if err := createAccountTables(ctx, tx); err != nil {
		return err
	}
	if err := migrateSettingsOwnership(ctx, tx); err != nil {
		return err
	}
	if err := migrateChatOwnership(ctx, tx); err != nil {
		return err
	}

	ownedTables := []struct {
		name       string
		createSQL  string
		oldColumns []string
	}{
		{
			name: "default_tools",
			createSQL: `CREATE TABLE default_tools_v24 (
				user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
				position INTEGER NOT NULL CHECK (position >= 0),
				name TEXT NOT NULL CHECK (name <> ''),
				PRIMARY KEY (user_id, position)
			) STRICT`,
			oldColumns: []string{"position", "name"},
		},
		{
			name: "prompt_appends",
			createSQL: `CREATE TABLE prompt_appends_v24 (
				user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
				id TEXT NOT NULL CHECK (id <> ''),
				position INTEGER NOT NULL CHECK (position >= 0),
				name TEXT NOT NULL CHECK (name <> ''),
				text TEXT NOT NULL CHECK (text <> ''),
				enabled_by_default INTEGER NOT NULL DEFAULT 0 CHECK (enabled_by_default IN (0, 1)),
				PRIMARY KEY (user_id, id),
				UNIQUE (user_id, position)
			) STRICT`,
			oldColumns: []string{"id", "position", "name", "text", "enabled_by_default"},
		},
		{
			name: "mcp_servers",
			createSQL: `CREATE TABLE mcp_servers_v24 (
				user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
				id TEXT NOT NULL CHECK (id <> ''),
				position INTEGER NOT NULL CHECK (position >= 0),
				name TEXT NOT NULL CHECK (name <> ''),
				url TEXT NOT NULL CHECK (url <> ''),
				authorization_token TEXT,
				PRIMARY KEY (user_id, id),
				UNIQUE (user_id, position)
			) STRICT`,
			oldColumns: []string{"id", "position", "name", "url", "authorization_token"},
		},
		{
			name: "model_endpoint_preferences",
			createSQL: `CREATE TABLE model_endpoint_preferences_v24 (
				user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
				model TEXT NOT NULL CHECK (trim(model) <> ''),
				endpoint_type TEXT NOT NULL CHECK (endpoint_type IN ('responses', 'messages', 'chat_completions')),
				PRIMARY KEY (user_id, model)
			) STRICT`,
			oldColumns: []string{"model", "endpoint_type"},
		},
	}
	for _, table := range ownedTables {
		if err := migrateOwnedTable(ctx, tx, table.name, table.createSQL, table.oldColumns); err != nil {
			return err
		}
	}
	return createAccountIndexesAndTriggers(ctx, tx)
}

// migrateLegacyAccountIdentity names the reserved legacy account Skrittle so a
// normal signup can claim it. Blank usernames are filled only when no other
// account already owns that name; a collision leaves the legacy row untouched
// instead of renaming or deleting the existing account. Case variants of the
// canonical name are normalized. Passwords, disabled state, IDs, settings, and
// child rows are never changed.
func migrateLegacyAccountIdentity(ctx context.Context, tx *sql.Tx) error {
	exists, err := tableExists(ctx, tx, "users")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE users
		SET username = ?
		WHERE id = ?
			AND username = ''
			AND NOT EXISTS (
				SELECT 1
				FROM users AS other
				WHERE other.id <> users.id
					AND other.username = ? COLLATE NOCASE
			)
	`, kritui_db.LegacyOwnerUsername, kritui_db.LegacyOwnerID, kritui_db.LegacyOwnerUsername); err != nil {
		return fmt.Errorf("name legacy account: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE users
		SET username = ?
		WHERE id = ?
			AND username <> ''
			AND username = ? COLLATE NOCASE
			AND username COLLATE BINARY <> ?
	`, kritui_db.LegacyOwnerUsername, kritui_db.LegacyOwnerID, kritui_db.LegacyOwnerUsername, kritui_db.LegacyOwnerUsername); err != nil {
		return fmt.Errorf("normalize legacy account name: %w", err)
	}
	return nil
}

func createAccountTables(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
			password_hash TEXT NOT NULL DEFAULT '',
			disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_username_unique
			ON users (username COLLATE NOCASE)
			WHERE username <> ''`,
		`INSERT OR IGNORE INTO users (id, username, password_hash, disabled)
			VALUES (1, '', '', 1)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY CHECK (token_hash <> ''),
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create account schema: %w", err)
		}
	}
	return nil
}

func migrateSettingsOwnership(ctx context.Context, tx *sql.Tx) error {
	exists, err := tableExists(ctx, tx, "settings")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE settings (
			id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			default_model TEXT,
			max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
			default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
			prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
			ntfy_endpoint TEXT,
			ntfy_topic TEXT,
			ntfy_api_key TEXT,
			llm_endpoint TEXT,
			llm_api_key TEXT,
			theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975')),
			tool_result_elision_mode TEXT NOT NULL DEFAULT 'none'
				CHECK (tool_result_elision_mode IN ('none', 'current_turn', 'last_user_turns', 'tool_result_budget', 'summarize')),
			tool_result_elision_user_turns INTEGER
				CHECK (tool_result_elision_user_turns IS NULL OR tool_result_elision_user_turns BETWEEN 1 AND 1000),
			tool_result_elision_token_budget INTEGER
				CHECK (tool_result_elision_token_budget IS NULL OR tool_result_elision_token_budget BETWEEN 1 AND 10000000),
			tool_result_elision_summary_model TEXT
				CHECK (tool_result_elision_summary_model IS NULL OR
					(trim(tool_result_elision_summary_model) <> '' AND length(CAST(tool_result_elision_summary_model AS BLOB)) <= 512))
		) STRICT`); err != nil {
			return fmt.Errorf("create user settings table: %w", err)
		}
	} else {
		referencesUsers, err := tableReferencesUsers(ctx, tx, "settings")
		if err != nil {
			return err
		}
		if !referencesUsers {
			if err := rebuildSettingsForUsers(ctx, tx); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO settings (id) VALUES (1)`); err != nil {
		return fmt.Errorf("ensure legacy owner settings: %w", err)
	}
	return nil
}

func rebuildSettingsForUsers(ctx context.Context, tx *sql.Tx) error {
	columns, err := tableColumns(ctx, tx, "settings")
	if err != nil {
		return err
	}
	if !columns["id"] {
		return errors.New("migrate user accounts: settings table has no id column")
	}
	knownColumns := []string{
		"id", "default_model", "max_tool_rounds", "default_tools_configured",
		"prompt_appends_configured", "ntfy_endpoint", "ntfy_topic", "ntfy_api_key",
		"llm_endpoint", "llm_api_key", "theme", "tool_result_elision_mode",
		"tool_result_elision_user_turns", "tool_result_elision_token_budget",
		"tool_result_elision_summary_model",
	}
	copied := make([]string, 0, len(knownColumns))
	for _, name := range knownColumns {
		if columns[name] {
			copied = append(copied, name)
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS settings_v24`); err != nil {
		return fmt.Errorf("remove stale settings rebuild table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE settings_v24 (
		id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		default_model TEXT,
		max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
		default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
		prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
		ntfy_endpoint TEXT,
		ntfy_topic TEXT,
		ntfy_api_key TEXT,
		llm_endpoint TEXT,
		llm_api_key TEXT,
		theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975')),
		tool_result_elision_mode TEXT NOT NULL DEFAULT 'none'
			CHECK (tool_result_elision_mode IN ('none', 'current_turn', 'last_user_turns', 'tool_result_budget', 'summarize')),
		tool_result_elision_user_turns INTEGER
			CHECK (tool_result_elision_user_turns IS NULL OR tool_result_elision_user_turns BETWEEN 1 AND 1000),
		tool_result_elision_token_budget INTEGER
			CHECK (tool_result_elision_token_budget IS NULL OR tool_result_elision_token_budget BETWEEN 1 AND 10000000),
		tool_result_elision_summary_model TEXT
			CHECK (tool_result_elision_summary_model IS NULL OR
				(trim(tool_result_elision_summary_model) <> '' AND length(CAST(tool_result_elision_summary_model AS BLOB)) <= 512))
	) STRICT`); err != nil {
		return fmt.Errorf("create rebuilt settings table: %w", err)
	}
	quoted := strings.Join(copied, ", ")
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings_v24 (`+quoted+`) SELECT `+quoted+` FROM settings`); err != nil {
		return fmt.Errorf("copy settings to user ownership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE settings`); err != nil {
		return fmt.Errorf("drop legacy settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE settings_v24 RENAME TO settings`); err != nil {
		return fmt.Errorf("rename user settings table: %w", err)
	}
	return nil
}

func migrateChatOwnership(ctx context.Context, tx *sql.Tx) error {
	exists, err := tableExists(ctx, tx, "chats")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
			title TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT`); err != nil {
			return fmt.Errorf("create user chats table: %w", err)
		}
		return nil
	}

	columns, err := tableColumns(ctx, tx, "chats")
	if err != nil {
		return err
	}
	if !columns["user_id"] {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE chats
			ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE
		`); err != nil {
			return fmt.Errorf("add chats.user_id: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET user_id = 1 WHERE user_id IS NULL`); err != nil {
		return fmt.Errorf("backfill legacy chat owner: %w", err)
	}
	return nil
}

func migrateOwnedTable(ctx context.Context, tx *sql.Tx, table, createSQL string, oldColumns []string) error {
	exists, err := tableExists(ctx, tx, table)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, canonicalOwnedCreateSQL(table)); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
		return nil
	}

	columns, err := tableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	if columns["user_id"] {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET user_id = 1 WHERE user_id IS NULL`); err != nil {
			return fmt.Errorf("backfill %s owner: %w", table, err)
		}
		return nil
	}

	temp := table + "_v24"
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+temp); err != nil {
		return fmt.Errorf("remove stale %s rebuild table: %w", temp, err)
	}
	if _, err := tx.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create rebuilt %s: %w", table, err)
	}

	copied := make([]string, 0, len(oldColumns))
	for _, name := range oldColumns {
		if columns[name] {
			copied = append(copied, name)
		}
	}
	if len(copied) == 0 {
		return fmt.Errorf("migrate user accounts: %s has no known columns", table)
	}
	quoted := strings.Join(copied, ", ")
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+temp+` (user_id, `+quoted+`)
		SELECT 1, `+quoted+`
		FROM `+table); err != nil {
		return fmt.Errorf("copy %s to user ownership: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
		return fmt.Errorf("drop legacy %s: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+temp+` RENAME TO `+table); err != nil {
		return fmt.Errorf("rename rebuilt %s: %w", table, err)
	}
	return nil
}

func canonicalOwnedCreateSQL(table string) string {
	switch table {
	case "default_tools":
		return `CREATE TABLE default_tools (
			user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			PRIMARY KEY (user_id, position)
		) STRICT`
	case "prompt_appends":
		return `CREATE TABLE prompt_appends (
			user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
			id TEXT NOT NULL CHECK (id <> ''),
			position INTEGER NOT NULL CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			text TEXT NOT NULL CHECK (text <> ''),
			enabled_by_default INTEGER NOT NULL DEFAULT 0 CHECK (enabled_by_default IN (0, 1)),
			PRIMARY KEY (user_id, id),
			UNIQUE (user_id, position)
		) STRICT`
	case "mcp_servers":
		return `CREATE TABLE mcp_servers (
			user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
			id TEXT NOT NULL CHECK (id <> ''),
			position INTEGER NOT NULL CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			url TEXT NOT NULL CHECK (url <> ''),
			authorization_token TEXT,
			PRIMARY KEY (user_id, id),
			UNIQUE (user_id, position)
		) STRICT`
	case "model_endpoint_preferences":
		return `CREATE TABLE model_endpoint_preferences (
			user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
			model TEXT NOT NULL CHECK (trim(model) <> ''),
			endpoint_type TEXT NOT NULL CHECK (endpoint_type IN ('responses', 'messages', 'chat_completions')),
			PRIMARY KEY (user_id, model)
		) STRICT`
	default:
		panic("unknown owned table " + table)
	}
}

func createAccountIndexesAndTriggers(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS chats_user_updated_idx
			ON chats (user_id, updated_at DESC, id DESC)`,
		`CREATE TRIGGER IF NOT EXISTS chats_assign_legacy_owner
		AFTER INSERT ON chats
		WHEN NEW.user_id IS NULL
		BEGIN
			UPDATE chats SET user_id = 1 WHERE id = NEW.id;
		END`,
		`CREATE TRIGGER IF NOT EXISTS chats_validate_user_update
		BEFORE UPDATE OF user_id ON chats
		WHEN NEW.user_id IS NULL OR NOT EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id)
		BEGIN
			SELECT RAISE(ABORT, 'chats.user_id must reference an existing user');
		END`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create user ownership trigger: %w", err)
		}
	}
	return nil
}

func tableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name = ?
	`, table).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect table %s: %w", table, err)
	}
	return count > 0, nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect columns for %s: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column for %s: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns for %s: %w", table, err)
	}
	return columns, nil
}

func tableReferencesUsers(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("inspect foreign keys for %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, sequence              int
			parent                    string
			from, to                  sql.NullString
			onUpdate, onDelete, match string
		)
		if err := rows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return false, fmt.Errorf("scan foreign key for %s: %w", table, err)
		}
		if parent == "users" && from.Valid && from.String == "id" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate foreign keys for %s: %w", table, err)
	}
	return false, nil
}
