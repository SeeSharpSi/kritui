package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func TestUserAccountsMigrationPreservesVersion23Ownership(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	if _, err := database.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
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
		) STRICT;
		INSERT INTO settings (
			id, default_model, max_tool_rounds, default_tools_configured,
			prompt_appends_configured, ntfy_endpoint, ntfy_topic, ntfy_api_key,
			llm_endpoint, llm_api_key, theme, tool_result_elision_mode,
			tool_result_elision_user_turns
		) VALUES (
			1, 'legacy-model', 9, 1, 1, 'https://ntfy.example', 'legacy-topic',
			'legacy-ntfy-secret', 'https://llm.example/v1/responses',
			'legacy-llm-secret', 'nord', 'last_user_turns', 3
		);
		CREATE TABLE default_tools (
			position INTEGER PRIMARY KEY CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> '')
		) STRICT;
		INSERT INTO default_tools (position, name) VALUES (0, 'webfetch');
		CREATE TABLE prompt_appends (
			id TEXT PRIMARY KEY CHECK (id <> ''),
			position INTEGER NOT NULL UNIQUE CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			text TEXT NOT NULL CHECK (text <> ''),
			enabled_by_default INTEGER NOT NULL DEFAULT 0 CHECK (enabled_by_default IN (0, 1))
		) STRICT;
		INSERT INTO prompt_appends (id, position, name, text, enabled_by_default)
			VALUES ('shared', 0, 'Shared', 'Legacy shared text.', 1);
		CREATE TABLE mcp_servers (
			id TEXT PRIMARY KEY CHECK (id <> ''),
			position INTEGER NOT NULL UNIQUE CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			url TEXT NOT NULL CHECK (url <> ''),
			authorization_token TEXT
		) STRICT;
		INSERT INTO mcp_servers (id, position, name, url, authorization_token)
			VALUES ('mcp-0123456789abcdef', 0, 'Legacy MCP', 'https://mcp.example', 'legacy-mcp-secret');
		CREATE TABLE model_endpoint_preferences (
			model TEXT PRIMARY KEY CHECK (trim(model) <> ''),
			endpoint_type TEXT NOT NULL CHECK (endpoint_type IN ('responses', 'messages', 'chat_completions'))
		) STRICT;
		INSERT INTO model_endpoint_preferences (model, endpoint_type)
			VALUES ('legacy-model', 'responses');
		CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT;
		INSERT INTO chats (id, title, created_at, updated_at)
			VALUES (7, 'legacy chat', '2026-01-01T00:00:00.000Z', '2026-01-02T00:00:00.000Z');
		CREATE TABLE chat_tools (
			chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			name TEXT NOT NULL CHECK (name <> ''),
			PRIMARY KEY (chat_id, position)
		) STRICT;
		INSERT INTO chat_tools (chat_id, position, name) VALUES (7, 0, 'webfetch');
		CREATE TABLE chat_prompt_appends (
			chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			prompt_append_id TEXT NOT NULL CHECK (prompt_append_id <> ''),
			PRIMARY KEY (chat_id, position)
		) STRICT;
		INSERT INTO chat_prompt_appends (chat_id, position, prompt_append_id) VALUES (7, 0, 'shared');
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			role TEXT NOT NULL CHECK (role IN ('system', 'developer', 'user', 'assistant', 'tool')),
			content TEXT NOT NULL DEFAULT '',
			model TEXT,
			total_tokens INTEGER,
			cost REAL,
			tool_call_id TEXT,
			undo_sequence INTEGER CHECK (undo_sequence IS NULL OR undo_sequence > 0),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			UNIQUE (chat_id, position)
		) STRICT;
		INSERT INTO messages (id, chat_id, position, role, content, undo_sequence)
			VALUES (10, 7, 0, 'user', 'kept question', NULL),
			       (11, 7, 1, 'assistant', 'hidden answer', 1);
		PRAGMA user_version = 23;
	`); err != nil {
		t.Fatalf("initialize version 23 fixture: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate user accounts: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 27 || version != len(databaseMigrations) {
		t.Fatalf("migrated version = %d, want 27 and %d", version, len(databaseMigrations))
	}

	var ownerDisabled int
	var ownerUsername, ownerHash string
	if err := database.QueryRow(`
		SELECT disabled, username, password_hash FROM users WHERE id = 1
	`).Scan(&ownerDisabled, &ownerUsername, &ownerHash); err != nil {
		t.Fatal(err)
	}
	if ownerDisabled != 1 || ownerUsername != kritui_db.LegacyOwnerUsername || ownerHash != "" {
		t.Fatalf("legacy owner = disabled %d, username %q, hash %q; want unclaimed Skrittle",
			ownerDisabled, ownerUsername, ownerHash)
	}

	var model, ntfyKey, llmKey, theme string
	var rounds, userTurns int
	if err := database.QueryRow(`
		SELECT default_model, max_tool_rounds, ntfy_api_key, llm_api_key, theme,
			tool_result_elision_user_turns
		FROM settings WHERE id = 1
	`).Scan(&model, &rounds, &ntfyKey, &llmKey, &theme, &userTurns); err != nil {
		t.Fatal(err)
	}
	if model != "legacy-model" || rounds != 9 || ntfyKey != "legacy-ntfy-secret" ||
		llmKey != "legacy-llm-secret" || theme != "nord" || userTurns != 3 {
		t.Fatalf("migrated owner settings lost data: model=%q rounds=%d ntfy=%q llm=%q theme=%q turns=%d",
			model, rounds, ntfyKey, llmKey, theme, userTurns)
	}

	var chatOwner int64
	var chatTitle, createdAt, updatedAt string
	if err := database.QueryRow(`SELECT user_id, title, created_at, updated_at FROM chats WHERE id = 7`).
		Scan(&chatOwner, &chatTitle, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if chatOwner != kritui_db.LegacyOwnerID || chatTitle != "legacy chat" ||
		createdAt != "2026-01-01T00:00:00.000Z" || updatedAt != "2026-01-02T00:00:00.000Z" {
		t.Fatalf("migrated chat = owner %d title %q timestamps %q/%q", chatOwner, chatTitle, createdAt, updatedAt)
	}
	var messageCount, hidden int
	if err := database.QueryRow(`SELECT COUNT(*), COUNT(undo_sequence) FROM messages WHERE chat_id = 7`).
		Scan(&messageCount, &hidden); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 || hidden != 1 {
		t.Fatalf("migrated messages = %d with %d hidden, want 2 and 1", messageCount, hidden)
	}

	for _, check := range []struct {
		query string
	}{
		{`SELECT user_id FROM default_tools WHERE name = 'webfetch'`},
		{`SELECT user_id FROM prompt_appends WHERE id = 'shared'`},
		{`SELECT user_id FROM mcp_servers WHERE id = 'mcp-0123456789abcdef'`},
		{`SELECT user_id FROM model_endpoint_preferences WHERE model = 'legacy-model'`},
	} {
		var owner int64
		if err := database.QueryRow(check.query).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != kritui_db.LegacyOwnerID {
			t.Errorf("%s owner = %d, want %d", check.query, owner, kritui_db.LegacyOwnerID)
		}
	}
	var mcpToken string
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = 'mcp-0123456789abcdef'`).Scan(&mcpToken); err != nil {
		t.Fatal(err)
	}
	if mcpToken != "legacy-mcp-secret" {
		t.Fatalf("migrated mcp token = %q, want preserved secret", mcpToken)
	}

	var settingsForeignKey int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM pragma_foreign_key_list('settings')
		WHERE "table" = 'users' AND "from" = 'id'
	`).Scan(&settingsForeignKey); err != nil {
		t.Fatal(err)
	}
	if settingsForeignKey != 1 {
		t.Fatalf("settings foreign keys to users = %d, want 1", settingsForeignKey)
	}

	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (8, 'raw legacy insert')`); err != nil {
		t.Fatalf("raw chat insert after migration: %v", err)
	}
	if err := database.QueryRow(`SELECT user_id FROM chats WHERE id = 8`).Scan(&chatOwner); err != nil || chatOwner != kritui_db.LegacyOwnerID {
		t.Fatalf("raw chat owner = %d, %v; want %d", chatOwner, err, kritui_db.LegacyOwnerID)
	}
	if _, err := database.Exec(`INSERT INTO default_tools (position, name) VALUES (1, 'raw-tool')`); err != nil {
		t.Fatalf("raw default tool insert after migration: %v", err)
	}
	if _, err := database.Exec(`UPDATE chats SET user_id = NULL WHERE id = 8`); err == nil {
		t.Fatal("clearing chat owner after migration error = nil, want trigger rejection")
	}

	ctx := context.Background()
	owner, err := kritui_db.CreateUser(ctx, database, "skrittle", "owner-hash")
	if err != nil {
		t.Fatalf("claim migrated owner: %v", err)
	}
	if owner.ID != kritui_db.LegacyOwnerID || owner.Username != kritui_db.LegacyOwnerUsername {
		t.Fatalf("claimed migrated owner = %#v", owner)
	}
	ownerCtx := kritui_db.WithUser(ctx, kritui_db.LegacyOwnerID)
	if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
		t.Fatalf("owner model after bootstrap = %q, %v", got, err)
	}
	second, err := kritui_db.CreateUser(ctx, database, "Second", "second-hash")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	secondCtx := kritui_db.WithUser(ctx, second.ID)
	if got, err := kritui_db.GetDefaultModel(secondCtx, database, "fallback"); err != nil || got != "fallback" {
		t.Fatalf("second user model = %q, %v, want fallback", got, err)
	}
	if servers, err := kritui_db.GetMCPServers(secondCtx, database); err != nil || len(servers) != 0 {
		t.Fatalf("second user mcp servers = %#v, %v, want none", servers, err)
	}
	if err := kritui_db.SaveSettings(secondCtx, database, kritui_db.SettingsUpdate{
		Model:         "second-model",
		MaxToolRounds: 5,
		PromptAppends: []kritui_db.PromptAppend{{ID: "shared", Name: "Second", Text: "Second user text."}},
		MCPServers: []kritui_db.MCPServerUpdate{{
			Server:              kritui_db.MCPServer{ID: "mcp-0123456789abcdef", Name: "Second MCP", URL: "https://second.example/mcp"},
			AuthorizationChange: kritui_db.MCPReplaceAuthorization,
			AuthorizationValue:  "second-token",
		}},
	}); err != nil {
		t.Fatalf("save second user settings with shared IDs: %v", err)
	}
	if got, err := kritui_db.GetPromptAppends(ownerCtx, database); err != nil ||
		!slices.Equal(got, []kritui_db.PromptAppend{{ID: "shared", Name: "Shared", Text: "Legacy shared text.", EnabledByDefault: true}}) {
		t.Fatalf("owner appends after second user save = %#v, %v", got, err)
	}
	if got, err := kritui_db.GetPromptAppends(secondCtx, database); err != nil ||
		!slices.Equal(got, []kritui_db.PromptAppend{{ID: "shared", Name: "Second", Text: "Second user text."}}) {
		t.Fatalf("second appends = %#v, %v", got, err)
	}
	if got, err := kritui_db.GetChatPromptAppendIDs(ownerCtx, database, 7); err != nil || !slices.Equal(got, []string{"shared"}) {
		t.Fatalf("owner chat appends after second user save = %v, %v, want preserved", got, err)
	}
	if got, err := kritui_db.GetChatTools(ownerCtx, database, 7); err != nil || !slices.Equal(got, []string{"webfetch"}) {
		t.Fatalf("owner chat tools after second user save = %v, %v, want preserved", got, err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUserAccounts(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatalf("repeat user migration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
		t.Fatalf("owner model after repeated migration = %q, %v", got, err)
	}
	if got, err := kritui_db.GetTheme(ownerCtx, database); err != nil || got != "nord" {
		t.Fatalf("owner theme after repeated migration = %q, %v", got, err)
	}
	var repeatedMessages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 7`).Scan(&repeatedMessages); err != nil || repeatedMessages != 2 {
		t.Fatalf("messages after repeated migration = %d, %v, want 2", repeatedMessages, err)
	}
	var secondTasks int
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings WHERE id = ?`, second.ID).Scan(&secondTasks); err != nil || secondTasks != 1 {
		t.Fatalf("second user settings rows = %d, %v", secondTasks, err)
	}
	if _, err := database.Exec(`INSERT INTO model_endpoint_preferences (user_id, model, endpoint_type)
		VALUES (?, 'second-model', 'chat_completions')`, second.ID); err != nil {
		t.Fatalf("insert second user protocol preference: %v", err)
	}
	if got, err := kritui_db.GetModelEndpointType(secondCtx, database, "second-model"); err != nil || got != llm.EndpointChatCompletions {
		t.Fatalf("second user protocol = %q, %v", got, err)
	}
	if got, err := kritui_db.GetModelEndpointType(ownerCtx, database, "second-model"); err != nil || got != "" {
		t.Fatalf("owner protocol for second model = %q, %v, want empty", got, err)
	}
}

func openVersion24TestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}
	if _, err := database.Exec(`PRAGMA user_version = 24`); err != nil {
		t.Fatalf("set version 24: %v", err)
	}
	return database
}

func TestLegacyAccountIdentityMigration(t *testing.T) {
	t.Run("unclaimed blank account", func(t *testing.T) {
		database := openVersion24TestDatabase(t)
		if _, err := database.Exec(`
			UPDATE users SET username = '', password_hash = '', disabled = 1 WHERE id = 1;
			UPDATE settings SET default_model = 'legacy-model', llm_api_key = 'legacy-key' WHERE id = 1;
			INSERT INTO chats (id, title) VALUES (7, 'legacy chat');
		`); err != nil {
			t.Fatalf("prepare version 24 database: %v", err)
		}
		if err := migrateDatabase(database); err != nil {
			t.Fatalf("migrate version 24 database: %v", err)
		}

		var version, disabled int
		var username, passwordHash string
		if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 27 {
			t.Fatalf("migrated version = %d, want 27", version)
		}
		if err := database.QueryRow(`
			SELECT username, password_hash, disabled FROM users WHERE id = 1
		`).Scan(&username, &passwordHash, &disabled); err != nil {
			t.Fatal(err)
		}
		if username != kritui_db.LegacyOwnerUsername || passwordHash != "" || disabled != 1 {
			t.Fatalf("migrated legacy account = %q hash %q disabled %d", username, passwordHash, disabled)
		}

		ctx := context.Background()
		ownerCtx := kritui_db.WithUser(ctx, kritui_db.LegacyOwnerID)
		if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
			t.Fatalf("legacy model after migration = %q, %v", got, err)
		}
		if config, err := kritui_db.GetLLMConfig(ownerCtx, database); err != nil || config.APIKey != "legacy-key" {
			t.Fatalf("legacy LLM key after migration = %q, %v", config.APIKey, err)
		}
		var chatOwner int64
		if err := database.QueryRow(`SELECT user_id FROM chats WHERE id = 7`).Scan(&chatOwner); err != nil {
			t.Fatal(err)
		}
		if chatOwner != kritui_db.LegacyOwnerID {
			t.Fatalf("legacy chat owner = %d, want %d", chatOwner, kritui_db.LegacyOwnerID)
		}

		owner, err := kritui_db.CreateUser(ctx, database, "SKRITTLE", "first-hash")
		if err != nil {
			t.Fatalf("claim migrated legacy account: %v", err)
		}
		if owner.ID != kritui_db.LegacyOwnerID || owner.Username != kritui_db.LegacyOwnerUsername ||
			owner.PasswordHash != "first-hash" {
			t.Fatalf("claimed migrated account = %#v", owner)
		}
		if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
			t.Fatalf("legacy model after claim = %q, %v", got, err)
		}
	})

	t.Run("initialized account", func(t *testing.T) {
		database := openVersion24TestDatabase(t)
		if _, err := database.Exec(`
			UPDATE users SET username = 'skrittle', password_hash = 'existing-hash', disabled = 0 WHERE id = 1;
		`); err != nil {
			t.Fatalf("prepare initialized version 24 database: %v", err)
		}
		if err := migrateDatabase(database); err != nil {
			t.Fatalf("migrate initialized version 24 database: %v", err)
		}

		ctx := context.Background()
		owner, err := kritui_db.GetUserByUsername(ctx, database, "SKRITTLE")
		if err != nil {
			t.Fatalf("get initialized legacy account: %v", err)
		}
		if owner.ID != kritui_db.LegacyOwnerID || owner.Username != kritui_db.LegacyOwnerUsername ||
			owner.PasswordHash != "existing-hash" {
			t.Fatalf("initialized legacy account changed: %#v", owner)
		}
		if _, err := kritui_db.CreateUser(ctx, database, "Skrittle", "replacement-hash"); !errors.Is(err, kritui_db.ErrUsernameTaken) {
			t.Fatalf("signup after initialized migration error = %v, want ErrUsernameTaken", err)
		}
		owner, err = kritui_db.GetUserByUsername(ctx, database, "skrittle")
		if err != nil || owner.PasswordHash != "existing-hash" {
			t.Fatalf("legacy credentials after rejected signup = %#v, %v", owner, err)
		}
	})

	t.Run("username collision", func(t *testing.T) {
		database := openVersion24TestDatabase(t)
		if _, err := database.Exec(`
			UPDATE users SET username = '', password_hash = '', disabled = 1 WHERE id = 1;
			INSERT INTO users (id, username, password_hash, disabled)
				VALUES (2, 'Skrittle', 'other-hash', 0);
			INSERT INTO settings (id) VALUES (2);
		`); err != nil {
			t.Fatalf("prepare colliding version 24 database: %v", err)
		}
		if err := migrateDatabase(database); err != nil {
			t.Fatalf("migrate colliding version 24 database: %v", err)
		}

		var legacyUsername, legacyHash string
		var legacyDisabled int
		if err := database.QueryRow(`
			SELECT username, password_hash, disabled FROM users WHERE id = 1
		`).Scan(&legacyUsername, &legacyHash, &legacyDisabled); err != nil {
			t.Fatal(err)
		}
		if legacyUsername != "" || legacyHash != "" || legacyDisabled != 1 {
			t.Fatalf("colliding legacy account = %q hash %q disabled %d, want untouched blank",
				legacyUsername, legacyHash, legacyDisabled)
		}
		var otherHash string
		if err := database.QueryRow(`SELECT password_hash FROM users WHERE id = 2`).Scan(&otherHash); err != nil {
			t.Fatal(err)
		}
		if otherHash != "other-hash" {
			t.Fatalf("colliding account hash = %q, want preserved", otherHash)
		}

		ctx := context.Background()
		if _, err := kritui_db.CreateUser(ctx, database, "skrittle", "claim-hash"); !errors.Is(err, kritui_db.ErrUsernameTaken) {
			t.Fatalf("signup with colliding username error = %v, want ErrUsernameTaken", err)
		}
		if owner, err := kritui_db.GetUserByUsername(ctx, database, "skrittle"); err != nil ||
			owner.ID != 2 || owner.PasswordHash != "other-hash" {
			t.Fatalf("colliding account after signup = %#v, %v", owner, err)
		}
	})
}
