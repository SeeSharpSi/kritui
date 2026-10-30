package kritui_db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

const shareTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func openShareTestDatabase(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	database := openMessagesTestDatabase(t, "")
	ctx := WithUser(context.Background(), LegacyOwnerID)
	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (1, 1, 'Share title');
		INSERT INTO messages (id, chat_id, position, role, content) VALUES
			(1, 1, 0, 'user', 'question'),
			(2, 1, 1, 'assistant', '<p>answer</p>');
	`); err != nil {
		t.Fatalf("insert share source: %v", err)
	}
	return database, ctx
}

func insertShareTestUser(t *testing.T, database *sql.DB, username string) int64 {
	t.Helper()
	user, err := CreateUser(context.Background(), database, username, "password-hash")
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	return user.ID
}

func shareTestEncryptedPayload() ([]byte, []byte) {
	nonce := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	ciphertext := make([]byte, 24)
	for index := range ciphertext {
		ciphertext[index] = byte(index)
	}
	ciphertext[0] = 0
	ciphertext[len(ciphertext)-1] = 0xff
	return nonce, ciphertext
}

func validShareTestResponse() SharedResponse {
	nonce, ciphertext := shareTestEncryptedPayload()
	return SharedResponse{
		TokenHash:     shareTestToken,
		ChatID:        1,
		MessageID:     2,
		FormatVersion: SharedResponseFormatVersion,
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	}
}

func countSharedResponseRows(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM shared_responses`).Scan(&count); err != nil {
		t.Fatalf("count shared responses: %v", err)
	}
	return count
}

func assertSharedResponseEqual(t *testing.T, got, want SharedResponse) {
	t.Helper()
	if got.TokenHash != want.TokenHash ||
		got.ChatID != want.ChatID ||
		got.MessageID != want.MessageID ||
		got.FormatVersion != want.FormatVersion {
		t.Errorf("SharedResponse metadata = %#v, want %#v", got, want)
	}
	if !bytes.Equal(got.Nonce, want.Nonce) {
		t.Errorf("SharedResponse nonce = %x, want %x", got.Nonce, want.Nonce)
	}
	if !bytes.Equal(got.Ciphertext, want.Ciphertext) {
		t.Errorf("SharedResponse ciphertext = %x, want %x", got.Ciphertext, want.Ciphertext)
	}
}

func TestGetShareSourceScopesAndFiltersMessages(t *testing.T) {
	t.Run("owned active assistant", func(t *testing.T) {
		database, ctx := openShareTestDatabase(t)
		source, err := GetShareSource(ctx, database, 1, 2)
		if err != nil {
			t.Fatalf("GetShareSource() error: %v", err)
		}
		if source.Title != "Share title" || source.UserContent != "question" || source.Content != "<p>answer</p>" {
			t.Fatalf("GetShareSource() = %#v, want stored title, preceding user content, and content", source)
		}
	})

	t.Run("foreign and mismatched IDs", func(t *testing.T) {
		database, _ := openShareTestDatabase(t)
		otherCtx := WithUser(context.Background(), insertShareTestUser(t, database, "alice"))
		cases := []struct {
			name      string
			ctx       context.Context
			chatID    int64
			messageID int64
		}{
			{name: "foreign owner", ctx: otherCtx, chatID: 1, messageID: 2},
			{name: "wrong chat", ctx: WithUser(context.Background(), LegacyOwnerID), chatID: 9, messageID: 2},
			{name: "user message", ctx: WithUser(context.Background(), LegacyOwnerID), chatID: 1, messageID: 1},
			{name: "missing message", ctx: WithUser(context.Background(), LegacyOwnerID), chatID: 1, messageID: 9},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				if _, err := GetShareSource(testCase.ctx, database, testCase.chatID, testCase.messageID); !errors.Is(err, ErrMessageNotFound) {
					t.Fatalf("GetShareSource() error = %v, want ErrMessageNotFound", err)
				}
			})
		}
	})

	t.Run("hidden message", func(t *testing.T) {
		database, ctx := openShareTestDatabase(t)
		if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = 2`); err != nil {
			t.Fatalf("hide message: %v", err)
		}
		if _, err := GetShareSource(ctx, database, 1, 2); !errors.Is(err, ErrMessageNotFound) {
			t.Fatalf("GetShareSource(hidden) error = %v, want ErrMessageNotFound", err)
		}
	})

	t.Run("message with tool calls", func(t *testing.T) {
		database, ctx := openShareTestDatabase(t)
		if _, err := database.Exec(`
			INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
			VALUES (2, 0, 'call-1', 'function', 'lookup', '{}')
		`); err != nil {
			t.Fatalf("insert tool call: %v", err)
		}
		if _, err := GetShareSource(ctx, database, 1, 2); !errors.Is(err, ErrMessageNotFound) {
			t.Fatalf("GetShareSource(tool call) error = %v, want ErrMessageNotFound", err)
		}
	})
}

func TestGetShareSourceSelectsNearestActiveUserMessage(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (10, 1, 'Nearest title');
		INSERT INTO messages (id, chat_id, position, role, content, tool_call_id) VALUES
			(100, 10, 0, 'user', 'older question', NULL),
			(101, 10, 1, 'user', 'target question', NULL),
			(102, 10, 2, 'assistant', 'calling tool', NULL),
			(103, 10, 3, 'tool', 'tool result', 'call-1'),
			(104, 10, 4, 'user', 'hidden closer question', NULL),
			(105, 10, 5, 'assistant', 'final answer', NULL),
			(106, 10, 6, 'user', 'later question', NULL);
		INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
		VALUES (102, 0, 'call-1', 'function', 'lookup', '{}');
		UPDATE messages SET undo_sequence = 1 WHERE id = 104;
	`); err != nil {
		t.Fatalf("insert nearest share source: %v", err)
	}

	source, err := GetShareSource(ctx, database, 10, 105)
	if err != nil {
		t.Fatalf("GetShareSource() error: %v", err)
	}
	if source.Title != "Nearest title" || source.UserContent != "target question" || source.Content != "final answer" {
		t.Fatalf("GetShareSource() = %#v, want nearest active user content and final answer", source)
	}

	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (11, 1, 'Legacy title');
		INSERT INTO messages (id, chat_id, position, role, content) VALUES
			(110, 11, 0, 'assistant', 'legacy answer');
	`); err != nil {
		t.Fatalf("insert legacy share source: %v", err)
	}
	legacy, err := GetShareSource(ctx, database, 11, 110)
	if err != nil {
		t.Fatalf("GetShareSource(legacy) error: %v", err)
	}
	if legacy.Title != "Legacy title" || legacy.UserContent != "" || legacy.Content != "legacy answer" {
		t.Fatalf("GetShareSource(legacy) = %#v, want empty preceding user content", legacy)
	}
}

func TestGetShareConversationFiltersHiddenAndToolMessagesAndScopesOwner(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO messages (id, chat_id, position, role, content, tool_call_id) VALUES
			(3, 1, 2, 'assistant', '', NULL),
			(4, 1, 3, 'tool', 'private tool result', 'call-1'),
			(5, 1, 4, 'user', 'hidden question', NULL),
			(6, 1, 5, 'assistant', 'visible answer', NULL);
		INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
		VALUES (3, 0, 'call-1', 'function', 'private', '{"secret":true}');
		UPDATE messages SET undo_sequence = 1 WHERE id = 5;
	`); err != nil {
		t.Fatalf("insert conversation fixtures: %v", err)
	}

	conversation, err := GetShareConversation(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetShareConversation() error: %v", err)
	}
	if conversation.Title != "Share title" || len(conversation.Messages) != 3 ||
		conversation.Messages[0] != (ShareConversationMessage{Role: "user", Content: "question"}) ||
		conversation.Messages[1] != (ShareConversationMessage{Role: "assistant", Content: "<p>answer</p>"}) ||
		conversation.Messages[2] != (ShareConversationMessage{Role: "assistant", Content: "visible answer"}) {
		t.Fatalf("GetShareConversation() = %#v, want active conversational text only", conversation)
	}
	foreign := WithUser(context.Background(), insertShareTestUser(t, database, "conversation_foreign"))
	if _, err := GetShareConversation(foreign, database, 1); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("GetShareConversation(foreign) error = %v, want ErrChatNotFound", err)
	}
}

func TestCreateSharedResponseValidatesPayload(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	base := validShareTestResponse()

	cases := []struct {
		name   string
		ctx    context.Context
		mutate func(*SharedResponse)
	}{
		{name: "invalid user", ctx: WithUser(context.Background(), 0), mutate: func(*SharedResponse) {}},
		{name: "empty token", ctx: ctx, mutate: func(share *SharedResponse) { share.TokenHash = "" }},
		{name: "whitespace token", ctx: ctx, mutate: func(share *SharedResponse) { share.TokenHash = "  " }},
		{name: "old format", ctx: ctx, mutate: func(share *SharedResponse) { share.FormatVersion = 0 }},
		{name: "unknown format", ctx: ctx, mutate: func(share *SharedResponse) { share.FormatVersion = SharedResponseFormatVersion + 1 }},
		{name: "missing nonce", ctx: ctx, mutate: func(share *SharedResponse) { share.Nonce = nil }},
		{name: "short nonce", ctx: ctx, mutate: func(share *SharedResponse) { share.Nonce = make([]byte, 11) }},
		{name: "long nonce", ctx: ctx, mutate: func(share *SharedResponse) { share.Nonce = make([]byte, 13) }},
		{name: "short ciphertext", ctx: ctx, mutate: func(share *SharedResponse) { share.Ciphertext = make([]byte, 15) }},
		{name: "oversized ciphertext", ctx: ctx, mutate: func(share *SharedResponse) {
			share.Ciphertext = make([]byte, MaxSharedResponseBytes+1)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			share := base
			testCase.mutate(&share)
			if err := CreateSharedResponse(testCase.ctx, database, share); err == nil {
				t.Fatal("CreateSharedResponse(invalid) error = nil")
			}
		})
	}
	if count := countSharedResponseRows(t, database); count != 0 {
		t.Fatalf("shared response rows after validation failures = %d, want 0", count)
	}

	minimum := base
	minimum.TokenHash = strings.Repeat("c", 64)
	minimum.Ciphertext = make([]byte, 16)
	if err := CreateSharedResponse(ctx, database, minimum); err != nil {
		t.Fatalf("CreateSharedResponse(minimum ciphertext) error: %v", err)
	}
}

func TestCreateSharedResponseOwnershipAndPublicLookup(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	aliceCtx := WithUser(context.Background(), insertShareTestUser(t, database, "alice"))
	share := validShareTestResponse()
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse() error: %v", err)
	}
	stored, err := GetSharedResponse(context.Background(), database, share.TokenHash)
	if err != nil {
		t.Fatalf("GetSharedResponse() error: %v", err)
	}
	assertSharedResponseEqual(t, stored, share)
	if _, err := GetSharedResponse(context.Background(), database, strings.Repeat("f", 64)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSharedResponse(missing) error = %v, want sql.ErrNoRows", err)
	}

	foreign := share
	foreign.TokenHash = strings.Repeat("a", 64)
	if err := CreateSharedResponse(aliceCtx, database, foreign); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("CreateSharedResponse(foreign) error = %v, want ErrMessageNotFound", err)
	}
	if count := countSharedResponseRows(t, database); count != 1 {
		t.Fatalf("shared response rows after foreign create = %d, want 1", count)
	}

	foreign.ChatID = 9
	if err := CreateSharedResponse(ctx, database, foreign); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("CreateSharedResponse(wrong chat) error = %v, want ErrMessageNotFound", err)
	}
	foreign.ChatID, foreign.MessageID = 1, 1
	if err := CreateSharedResponse(ctx, database, foreign); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("CreateSharedResponse(user message) error = %v, want ErrMessageNotFound", err)
	}
	if count := countSharedResponseRows(t, database); count != 1 {
		t.Fatalf("shared response rows after rejected sources = %d, want 1", count)
	}
}

func TestCreateSharedResponseRejectsHiddenAndToolCallSources(t *testing.T) {
	t.Run("hidden", func(t *testing.T) {
		database, ctx := openShareTestDatabase(t)
		if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = 2`); err != nil {
			t.Fatalf("hide message: %v", err)
		}
		defer func() {
			if _, err := database.Exec(`UPDATE messages SET undo_sequence = NULL WHERE id = 2`); err != nil {
				t.Fatalf("restore message: %v", err)
			}
		}()
		if err := CreateSharedResponse(ctx, database, validShareTestResponse()); !errors.Is(err, ErrMessageNotFound) {
			t.Fatalf("CreateSharedResponse(hidden) error = %v, want ErrMessageNotFound", err)
		}
	})

	t.Run("tool calls", func(t *testing.T) {
		database, ctx := openShareTestDatabase(t)
		if _, err := database.Exec(`
			INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
			VALUES (2, 0, 'call-1', 'function', 'lookup', '{}')
		`); err != nil {
			t.Fatalf("insert tool call: %v", err)
		}
		if err := CreateSharedResponse(ctx, database, validShareTestResponse()); !errors.Is(err, ErrMessageNotFound) {
			t.Fatalf("CreateSharedResponse(tool call) error = %v, want ErrMessageNotFound", err)
		}
	})
}

func TestCreateSharedResponseIsIdempotentAndRejectsConflicts(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	aliceID := insertShareTestUser(t, database, "alice")
	aliceCtx := WithUser(context.Background(), aliceID)
	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (5, ?, 'Alice chat');
		INSERT INTO messages (id, chat_id, position, role, content) VALUES (5, 5, 0, 'assistant', 'alice answer');
	`, aliceID); err != nil {
		t.Fatalf("insert alice source: %v", err)
	}

	share := validShareTestResponse()
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse() error: %v", err)
	}
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse(identical retry) error: %v", err)
	}
	if count := countSharedResponseRows(t, database); count != 1 {
		t.Fatalf("shared response rows after identical retry = %d, want 1", count)
	}

	conflicts := []struct {
		name   string
		ctx    context.Context
		mutate func(*SharedResponse)
	}{
		{name: "different ciphertext", ctx: ctx, mutate: func(candidate *SharedResponse) {
			candidate.Ciphertext = append([]byte(nil), candidate.Ciphertext...)
			candidate.Ciphertext[0] ^= 0xff
		}},
		{name: "different nonce", ctx: ctx, mutate: func(candidate *SharedResponse) {
			candidate.Nonce = append([]byte(nil), candidate.Nonce...)
			candidate.Nonce[0] ^= 0xff
		}},
		{name: "different source", ctx: ctx, mutate: func(candidate *SharedResponse) {
			candidate.ChatID, candidate.MessageID = 5, 5
		}},
		{name: "different owner", ctx: aliceCtx, mutate: func(candidate *SharedResponse) {
			candidate.ChatID, candidate.MessageID = 5, 5
		}},
	}
	for _, testCase := range conflicts {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := share
			testCase.mutate(&candidate)
			if err := CreateSharedResponse(testCase.ctx, database, candidate); !errors.Is(err, ErrShareConflict) {
				t.Fatalf("CreateSharedResponse(conflict) error = %v, want ErrShareConflict", err)
			}
		})
	}
	stored, err := GetSharedResponse(context.Background(), database, share.TokenHash)
	if err != nil {
		t.Fatalf("GetSharedResponse() after conflicts error: %v", err)
	}
	assertSharedResponseEqual(t, stored, share)

	if _, err := database.Exec(`DELETE FROM chats WHERE id = 1`); err != nil {
		t.Fatalf("delete source chat: %v", err)
	}
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse(identical retry after source deletion) error: %v", err)
	}
	if count := countSharedResponseRows(t, database); count != 1 {
		t.Fatalf("shared response rows after deleted-source retry = %d, want 1", count)
	}
	conflicting := share
	conflicting.Ciphertext = append([]byte(nil), share.Ciphertext...)
	conflicting.Ciphertext[0] ^= 0xff
	if err := CreateSharedResponse(ctx, database, conflicting); !errors.Is(err, ErrShareConflict) {
		t.Fatalf("CreateSharedResponse(conflict after source deletion) error = %v, want ErrShareConflict", err)
	}
	newToken := share
	newToken.TokenHash = strings.Repeat("d", 64)
	if err := CreateSharedResponse(ctx, database, newToken); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("CreateSharedResponse(new token after source deletion) error = %v, want ErrMessageNotFound", err)
	}
}

func TestSharedResponseSnapshotSurvivesSourceRemoval(t *testing.T) {
	database, ctx := openShareTestDatabase(t)
	share := validShareTestResponse()
	share.TokenHash = strings.Repeat("e", 64)
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse() error: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE messages SET content = '<p>edited</p>' WHERE id = 2;
		DELETE FROM chats WHERE id = 1;
	`); err != nil {
		t.Fatalf("edit and delete source: %v", err)
	}
	stored, err := GetSharedResponse(context.Background(), database, share.TokenHash)
	if err != nil {
		t.Fatalf("GetSharedResponse(after source removal) error: %v", err)
	}
	assertSharedResponseEqual(t, stored, share)
	var chatID, messageID int64
	if err := database.QueryRow(`
		SELECT chat_id, message_id FROM shared_responses WHERE token_hash = ?
	`, share.TokenHash).Scan(&chatID, &messageID); err != nil {
		t.Fatalf("read share metadata: %v", err)
	}
	if chatID != share.ChatID || messageID != share.MessageID {
		t.Fatalf("share metadata = %d/%d, want %d/%d", chatID, messageID, share.ChatID, share.MessageID)
	}

	aliceID := insertShareTestUser(t, database, "alice")
	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (5, ?, 'Alice chat');
		INSERT INTO messages (id, chat_id, position, role, content) VALUES (5, 5, 0, 'assistant', 'alice answer');
	`, aliceID); err != nil {
		t.Fatalf("insert alice source: %v", err)
	}
	aliceShare := validShareTestResponse()
	aliceShare.TokenHash = strings.Repeat("b", 64)
	aliceShare.ChatID, aliceShare.MessageID = 5, 5
	if err := CreateSharedResponse(WithUser(context.Background(), aliceID), database, aliceShare); err != nil {
		t.Fatalf("CreateSharedResponse(alice) error: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM users WHERE id = ?`, aliceID); err != nil {
		t.Fatalf("delete alice: %v", err)
	}
	if _, err := GetSharedResponse(context.Background(), database, aliceShare.TokenHash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSharedResponse(deleted owner) error = %v, want sql.ErrNoRows", err)
	}
}

func TestEncryptedSharedResponseSchemaFilesMatch(t *testing.T) {
	start := strings.Index(messagesTestSchema, "CREATE TABLE IF NOT EXISTS shared_responses")
	if start < 0 {
		t.Fatal("schema.sql has no shared_responses definition")
	}
	end := strings.Index(messagesTestSchema[start:], "CREATE TABLE IF NOT EXISTS inbox_items")
	if end < 0 {
		t.Fatal("schema.sql does not place shared_responses before inbox_items")
	}
	embedded := strings.TrimSpace(messagesTestSchema[start : start+end])
	if embedded != strings.TrimSpace(encryptedShareSchema) {
		t.Fatalf("schema.sql shared block differs from encrypted_share_schema.sql:\n%s\n---\n%s", embedded, encryptedShareSchema)
	}
	if !strings.Contains(shareSchema, "html TEXT NOT NULL") || !strings.Contains(shareSchema, "theme TEXT NOT NULL") {
		t.Fatal("legacy share_schema.sql no longer contains the version 29 plaintext columns")
	}
	if strings.Contains(encryptedShareSchema, "html") || strings.Contains(encryptedShareSchema, "theme") {
		t.Fatal("encrypted_share_schema.sql contains legacy plaintext columns")
	}
}

func legacyShareVersion28Fixture(t *testing.T) string {
	t.Helper()
	start := strings.Index(messagesTestSchema, "CREATE TABLE IF NOT EXISTS shared_responses")
	if start < 0 {
		t.Fatal("schema fixture has no shared_responses block")
	}
	end := strings.Index(messagesTestSchema[start:], "CREATE TABLE IF NOT EXISTS inbox_items")
	if end < 0 {
		t.Fatal("schema fixture has no inbox_items block after shared_responses")
	}
	fixture := messagesTestSchema[:start] + messagesTestSchema[start+end:]
	fixture = strings.Replace(fixture, "PRAGMA user_version = 34;", "PRAGMA user_version = 28;", 1)
	fixture = strings.Replace(fixture, "    system_prompt TEXT,\n", "", 1)
	fixture = strings.Replace(fixture, "    compaction_prompt TEXT,\n", "", 1)
	if !strings.Contains(fixture, "PRAGMA user_version = 28;") {
		t.Fatal("version 28 fixture marker is missing")
	}
	return fixture
}

func legacyShareVersion29Fixture(t *testing.T) string {
	t.Helper()
	start := strings.Index(messagesTestSchema, "CREATE TABLE IF NOT EXISTS shared_responses")
	if start < 0 {
		t.Fatal("schema fixture has no shared_responses block")
	}
	end := strings.Index(messagesTestSchema[start:], "CREATE TABLE IF NOT EXISTS inbox_items")
	if end < 0 {
		t.Fatal("schema fixture has no inbox_items block after shared_responses")
	}
	fixture := messagesTestSchema[:start] +
		strings.TrimSpace(shareSchema) + "\n\n" +
		messagesTestSchema[start+end:]
	fixture = strings.Replace(fixture, "PRAGMA user_version = 34;", "PRAGMA user_version = 29;", 1)
	fixture = strings.Replace(fixture, "    system_prompt TEXT,\n", "", 1)
	fixture = strings.Replace(fixture, "    compaction_prompt TEXT,\n", "", 1)
	if !strings.Contains(fixture, "PRAGMA user_version = 29;") {
		t.Fatal("version 29 fixture marker is missing")
	}
	return fixture
}

func TestMigrateSharedResponsesFromVersion28(t *testing.T) {
	database := openMessagesTestConnection(t, "")
	if _, err := database.Exec(legacyShareVersion28Fixture(t)); err != nil {
		t.Fatalf("initialize version 28 fixture: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'shared_responses'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("shared_responses before migration count = %d, %v, want 0", count, err)
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin shared response migration: %v", err)
	}
	if err := MigrateSharedResponses(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatalf("MigrateSharedResponses() error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit shared response migration: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 28 {
		t.Fatalf("version after MigrateSharedResponses = %d, %v, want caller to record 29", version, err)
	}

	var htmlColumns, themeColumns int
	if err := database.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE name = 'html'),
			COUNT(*) FILTER (WHERE name = 'theme')
		FROM pragma_table_info('shared_responses')
	`).Scan(&htmlColumns, &themeColumns); err != nil {
		t.Fatalf("inspect legacy shared responses columns: %v", err)
	}
	if htmlColumns != 1 || themeColumns != 1 {
		t.Fatalf("legacy html/theme columns = %d/%d, want 1/1", htmlColumns, themeColumns)
	}
	if _, err := database.Exec(`
		INSERT INTO shared_responses (token_hash, user_id, chat_id, message_id, theme, html)
		VALUES ('legacy-token', 1, 1, 1, '1975', '<p>legacy</p>')
	`); err != nil {
		t.Fatalf("insert legacy share: %v", err)
	}

	tx, err = database.Begin()
	if err != nil {
		t.Fatalf("begin repeated legacy migration: %v", err)
	}
	if err := MigrateSharedResponses(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatalf("repeated MigrateSharedResponses() error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit repeated legacy migration: %v", err)
	}
	var legacyHTML string
	if err := database.QueryRow(`SELECT html FROM shared_responses WHERE token_hash = 'legacy-token'`).Scan(&legacyHTML); err != nil || legacyHTML != "<p>legacy</p>" {
		t.Fatalf("legacy row after repeated migration = %q, %v, want preserved", legacyHTML, err)
	}

	reference := openMessagesTestConnection(t, "")
	if _, err := reference.Exec(shareSchema); err != nil {
		t.Fatalf("create reference legacy schema: %v", err)
	}
	var migratedDDL, referenceDDL string
	if err := database.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'shared_responses'`).Scan(&migratedDDL); err != nil {
		t.Fatalf("read migrated DDL: %v", err)
	}
	if err := reference.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'shared_responses'`).Scan(&referenceDDL); err != nil {
		t.Fatalf("read reference DDL: %v", err)
	}
	if strings.TrimSpace(migratedDDL) != strings.TrimSpace(referenceDDL) {
		t.Fatalf("migrated legacy DDL differs from share_schema.sql DDL:\n%s\n---\n%s", migratedDDL, referenceDDL)
	}
}

func TestMigrateEncryptedSharedResponsesRetiresVersion29Plaintext(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		pragma string
		value  int
	}{
		{name: "secure_delete_off", pragma: "OFF", value: 0},
		{name: "secure_delete_on", pragma: "ON", value: 1},
		{name: "secure_delete_fast", pragma: "FAST", value: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openMessagesTestConnection(t, "")
			if _, err := database.Exec(legacyShareVersion29Fixture(t)); err != nil {
				t.Fatalf("initialize version 29 fixture: %v", err)
			}
			if _, err := database.Exec(`
		INSERT INTO users (id, username, password_hash) VALUES (2, 'alice', 'alice-hash');
		INSERT INTO settings (id, default_model, theme) VALUES (2, 'alice-model', 'nord');
		INSERT INTO chats (id, user_id, title) VALUES
			(1, 1, 'Legacy chat'),
			(2, 2, 'Alice chat');
		INSERT INTO messages (id, chat_id, position, role, content) VALUES
			(1, 1, 0, 'user', 'legacy question'),
			(2, 1, 1, 'assistant', 'legacy answer'),
			(3, 2, 0, 'assistant', 'alice answer');
		INSERT INTO shared_responses (token_hash, user_id, chat_id, message_id, theme, html)
		VALUES
			('legacy-share-one', 1, 1, 2, 'nord', '<p>legacy secret one</p>'),
			('legacy-share-two', 2, 2, 3, '1975', '<p>legacy secret two</p>');
	`); err != nil {
				t.Fatalf("seed version 29 data: %v", err)
			}
			for _, statement := range []string{
				`SELECT COUNT(*) FROM users`,
				`SELECT COUNT(*) FROM chats`,
				`SELECT COUNT(*) FROM messages`,
				`SELECT COUNT(*) FROM settings`,
			} {
				var count int
				if err := database.QueryRow(statement).Scan(&count); err != nil {
					t.Fatalf("count before migration: %v", err)
				}
			}
			if _, err := database.Exec(`PRAGMA secure_delete = ` + testCase.pragma); err != nil {
				t.Fatalf("set initial secure_delete mode: %v", err)
			}
			var initialSecureDeleteMode int
			if err := database.QueryRow(`PRAGMA secure_delete`).Scan(&initialSecureDeleteMode); err != nil || initialSecureDeleteMode != testCase.value {
				t.Fatalf("initial secure_delete mode = %d, %v, want %d", initialSecureDeleteMode, err, testCase.value)
			}

			tx, err := database.Begin()
			if err != nil {
				t.Fatalf("begin encrypted share migration: %v", err)
			}
			if err := MigrateEncryptedSharedResponses(context.Background(), tx); err != nil {
				tx.Rollback()
				t.Fatalf("MigrateEncryptedSharedResponses() error: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit encrypted share migration: %v", err)
			}
			var version int
			if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 29 {
				t.Fatalf("version after MigrateEncryptedSharedResponses = %d, %v, want caller to record 30", version, err)
			}
			var secureDeleteMode int
			if err := database.QueryRow(`PRAGMA secure_delete`).Scan(&secureDeleteMode); err != nil || secureDeleteMode != testCase.value {
				t.Fatalf("secure_delete mode after migration = %d, %v, want restored %d", secureDeleteMode, err, testCase.value)
			}

			var nonceColumns, ciphertextColumns, formatColumns, htmlColumns int
			if err := database.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE name = 'nonce'),
			COUNT(*) FILTER (WHERE name = 'ciphertext'),
			COUNT(*) FILTER (WHERE name = 'format_version'),
			COUNT(*) FILTER (WHERE name = 'html')
		FROM pragma_table_info('shared_responses')
	`).Scan(&nonceColumns, &ciphertextColumns, &formatColumns, &htmlColumns); err != nil {
				t.Fatalf("inspect encrypted schema columns: %v", err)
			}
			if nonceColumns != 1 || ciphertextColumns != 1 || formatColumns != 1 || htmlColumns != 0 {
				t.Fatalf("encrypted columns nonce/ciphertext/format/html = %d/%d/%d/%d, want 1/1/1/0",
					nonceColumns, ciphertextColumns, formatColumns, htmlColumns)
			}
			if count := countSharedResponseRows(t, database); count != 0 {
				t.Fatalf("shared response rows after retirement = %d, want legacy rows deleted", count)
			}
			if _, err := GetSharedResponse(context.Background(), database, "legacy-share-one"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("GetSharedResponse(retired legacy token) error = %v, want sql.ErrNoRows", err)
			}

			var legacyChatTitle, aliceChatTitle, legacyContent, aliceContent, aliceModel, aliceTheme string
			var userCount, chatCount, messageCount, settingCount int
			if err := database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
				t.Fatalf("count users after migration: %v", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chatCount); err != nil {
				t.Fatalf("count chats after migration: %v", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
				t.Fatalf("count messages after migration: %v", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&settingCount); err != nil {
				t.Fatalf("count settings after migration: %v", err)
			}
			if userCount != 2 || chatCount != 2 || messageCount != 3 || settingCount != 2 {
				t.Fatalf("preserved row counts users/chats/messages/settings = %d/%d/%d/%d, want 2/2/3/2",
					userCount, chatCount, messageCount, settingCount)
			}
			if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&legacyChatTitle); err != nil || legacyChatTitle != "Legacy chat" {
				t.Fatalf("legacy chat after migration = %q, %v", legacyChatTitle, err)
			}
			if err := database.QueryRow(`SELECT title FROM chats WHERE id = 2`).Scan(&aliceChatTitle); err != nil || aliceChatTitle != "Alice chat" {
				t.Fatalf("alice chat after migration = %q, %v", aliceChatTitle, err)
			}
			if err := database.QueryRow(`SELECT content FROM messages WHERE id = 2`).Scan(&legacyContent); err != nil || legacyContent != "legacy answer" {
				t.Fatalf("legacy message after migration = %q, %v", legacyContent, err)
			}
			if err := database.QueryRow(`SELECT content FROM messages WHERE id = 3`).Scan(&aliceContent); err != nil || aliceContent != "alice answer" {
				t.Fatalf("alice message after migration = %q, %v", aliceContent, err)
			}
			if err := database.QueryRow(`SELECT default_model, theme FROM settings WHERE id = 2`).Scan(&aliceModel, &aliceTheme); err != nil || aliceModel != "alice-model" || aliceTheme != "nord" {
				t.Fatalf("alice settings after migration = %q/%q, %v", aliceModel, aliceTheme, err)
			}
			var inboxCount int
			if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'inbox_items'`).Scan(&inboxCount); err != nil || inboxCount != 1 {
				t.Fatalf("inbox table after migration count = %d, %v, want 1", inboxCount, err)
			}

			share := validShareTestResponse()
			if err := CreateSharedResponse(WithUser(context.Background(), LegacyOwnerID), database, share); err != nil {
				t.Fatalf("CreateSharedResponse() after migration error: %v", err)
			}

			tx, err = database.Begin()
			if err != nil {
				t.Fatalf("begin repeated encrypted share migration: %v", err)
			}
			if err := MigrateEncryptedSharedResponses(context.Background(), tx); err != nil {
				tx.Rollback()
				t.Fatalf("repeated MigrateEncryptedSharedResponses() error: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit repeated encrypted share migration: %v", err)
			}
			if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 29 {
				t.Fatalf("version after repeated migration = %d, %v, want caller to record 30", version, err)
			}
			stored, err := GetSharedResponse(context.Background(), database, share.TokenHash)
			if err != nil {
				t.Fatalf("GetSharedResponse() after repeated migration error: %v", err)
			}
			assertSharedResponseEqual(t, stored, share)
			if count := countSharedResponseRows(t, database); count != 1 {
				t.Fatalf("shared response rows after repeated migration = %d, want preserved encrypted row", count)
			}
		})
	}
}

func TestMigrateEncryptedSharedResponsesRejectsUnexpectedShape(t *testing.T) {
	cases := []struct {
		name   string
		ddl    string
		insert string
	}{
		{
			name: "unrelated columns",
			ddl: `CREATE TABLE shared_responses (
				token_hash TEXT PRIMARY KEY,
				note TEXT NOT NULL
			) STRICT;`,
			insert: `INSERT INTO shared_responses (token_hash, note) VALUES ('held', 'keep me')`,
		},
		{
			name: "partial encrypted columns",
			ddl: `CREATE TABLE shared_responses (
				token_hash TEXT PRIMARY KEY,
				note TEXT NOT NULL,
				format_version INTEGER NOT NULL,
				nonce BLOB NOT NULL
			) STRICT;`,
			insert: `INSERT INTO shared_responses (token_hash, note, format_version, nonce)
				VALUES ('held', 'keep me', 1, zeroblob(12))`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			database := openMessagesTestConnection(t, "")
			if _, err := database.Exec(testCase.ddl); err != nil {
				t.Fatalf("create unexpected shared_responses shape: %v", err)
			}
			if _, err := database.Exec(testCase.insert); err != nil {
				t.Fatalf("seed unexpected shared_responses row: %v", err)
			}
			var beforeDDL string
			if err := database.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'shared_responses'`).Scan(&beforeDDL); err != nil {
				t.Fatalf("read unexpected DDL: %v", err)
			}

			tx, err := database.Begin()
			if err != nil {
				t.Fatalf("begin unexpected shape migration: %v", err)
			}
			err = MigrateEncryptedSharedResponses(context.Background(), tx)
			if err == nil || !strings.Contains(err.Error(), "unexpected shape") {
				tx.Rollback()
				t.Fatalf("MigrateEncryptedSharedResponses(unexpected shape) error = %v, want unexpected shape error", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback unexpected shape migration: %v", err)
			}

			var afterDDL, note string
			if err := database.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'shared_responses'`).Scan(&afterDDL); err != nil {
				t.Fatalf("read preserved DDL: %v", err)
			}
			if afterDDL != beforeDDL {
				t.Fatalf("unexpected-shape DDL changed:\n%s\n---\n%s", beforeDDL, afterDDL)
			}
			if err := database.QueryRow(`SELECT note FROM shared_responses WHERE token_hash = 'held'`).Scan(&note); err != nil || note != "keep me" {
				t.Fatalf("preserved unexpected-shape row = %q, %v, want keep me", note, err)
			}
			var ciphertextColumns int
			if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('shared_responses') WHERE name = 'ciphertext'`).Scan(&ciphertextColumns); err != nil || ciphertextColumns != 0 {
				t.Fatalf("unexpected-shape ciphertext columns = %d, %v, want 0", ciphertextColumns, err)
			}
		})
	}
}
