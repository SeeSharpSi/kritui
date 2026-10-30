package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func storageTestKey(fill byte) []byte {
	key := make([]byte, kritui_db.StorageKeySize)
	for index := range key {
		key[index] = fill + byte(index)
	}
	return key
}

func newStorageTestCipher(t *testing.T, fill byte) *kritui_db.StorageCipher {
	t.Helper()
	storage, err := kritui_db.NewStorageCipher(storageTestKey(fill))
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	return storage
}

func openStorageTestDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	database, err := openDatabase(path)
	if err != nil {
		t.Fatalf("openDatabase() error: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	return database, path
}

func storageTestContext(storage *kritui_db.StorageCipher, ownerID int64) context.Context {
	return kritui_db.WithStorageCipher(kritui_db.WithUser(context.Background(), ownerID), storage)
}

func requireEncryptedText(t *testing.T, value, plaintext string) {
	t.Helper()
	if value == plaintext {
		t.Fatalf("stored value equals plaintext %q", plaintext)
	}
	if !strings.HasPrefix(value, "enc:v1:") {
		t.Fatalf("stored value = %q, want enc:v1 envelope", value)
	}
}

func decryptStorageTextForTest(t *testing.T, storage *kritui_db.StorageCipher, ownerID int64, scope, rowID, value string) string {
	t.Helper()
	plaintext, err := kritui_db.DecryptStorageText(storageTestContext(storage, ownerID), scope, rowID, value)
	if err != nil {
		t.Fatalf("DecryptStorageText(%s, %s) error: %v", scope, rowID, err)
	}
	return plaintext
}

func decryptStorageBytesForTest(t *testing.T, storage *kritui_db.StorageCipher, ownerID int64, scope, rowID string, value []byte) []byte {
	t.Helper()
	plaintext, err := kritui_db.DecryptStorageBytes(storageTestContext(storage, ownerID), scope, rowID, value)
	if err != nil {
		t.Fatalf("DecryptStorageBytes(%s, %s) error: %v", scope, rowID, err)
	}
	return plaintext
}

func TestStorageEncryptionKeyFromEnvironment(t *testing.T) {
	validKey := storageTestKey(1)
	valid := base64.StdEncoding.EncodeToString(validKey)

	t.Setenv(storageEncryptionKeyEnvironment, valid)
	key, err := storageEncryptionKeyFromEnvironment()
	if err != nil {
		t.Fatalf("storageEncryptionKeyFromEnvironment() error: %v", err)
	}
	if !bytes.Equal(key, validKey) {
		t.Fatalf("decoded key = %x, want %x", key, validKey)
	}

	t.Setenv(storageEncryptionKeyEnvironment, " \t\r\n"+valid+"\n")
	key, err = storageEncryptionKeyFromEnvironment()
	if err != nil {
		t.Fatalf("storageEncryptionKeyFromEnvironment() with surrounding whitespace error: %v", err)
	}
	if !bytes.Equal(key, validKey) {
		t.Fatalf("decoded trimmed key = %x, want %x", key, validKey)
	}

	embedded := valid[:20] + "\n" + valid[20:]
	t.Setenv(storageEncryptionKeyEnvironment, embedded)
	if _, err := storageEncryptionKeyFromEnvironment(); err == nil {
		t.Fatal("storageEncryptionKeyFromEnvironment() accepted embedded whitespace")
	}

	short := base64.StdEncoding.EncodeToString(storageTestKey(1)[:31])
	for name, value := range map[string]string{
		"empty":           "",
		"short key":       short,
		"invalid base64":  "not-base64!",
		"missing padding": valid[:len(valid)-1],
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(storageEncryptionKeyEnvironment, value)
			if _, err := storageEncryptionKeyFromEnvironment(); err == nil {
				t.Fatalf("storageEncryptionKeyFromEnvironment() accepted %q", value)
			}
		})
	}
}

func TestMissingStorageEncryptionKeyDoesNotTouchDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(storageEncryptionKeyEnvironment, "placeholder")
	if err := os.Unsetenv(storageEncryptionKeyEnvironment); err != nil {
		t.Fatalf("unset key: %v", err)
	}

	if _, err := storageEncryptionCipherFromEnvironment(); err == nil {
		t.Fatal("storageEncryptionCipherFromEnvironment() without a key error = nil")
	}
	if _, err := os.Stat("data.db"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data.db stat error = %v, want fs.ErrNotExist", err)
	}
}

func TestRunRequiresKeyBeforeDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(storageEncryptionKeyEnvironment, "placeholder")
	if err := os.Unsetenv(storageEncryptionKeyEnvironment); err != nil {
		t.Fatalf("unset key: %v", err)
	}

	if err := Run(nil, ""); err == nil {
		t.Fatal("Run() without a key error = nil")
	}
	if _, err := os.Stat("data.db"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run() touched data.db without a key: %v", err)
	}
}

func TestStorageEncryptionFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	database, err := openDatabase(path)
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	defer database.Close()

	storage := newStorageTestCipher(t, 3)
	if err := ensureStorageEncryption(context.Background(), database, storage, false); err != nil {
		t.Fatalf("ensureStorageEncryption(fresh) error: %v", err)
	}

	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user version: %v", err)
	}
	if len(databaseMigrations) != 34 {
		t.Fatalf("databaseMigrations length = %d, want 34 compaction prompt version", len(databaseMigrations))
	}
	if version != 34 {
		t.Errorf("user_version = %d, want 34", version)
	}
	var secureDelete int
	if err := database.QueryRow(`PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
		t.Fatalf("read secure_delete: %v", err)
	}
	if secureDelete != 1 {
		t.Errorf("secure_delete = %d, want 1", secureDelete)
	}
	var tempStore int
	if err := database.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil {
		t.Fatalf("read temp_store: %v", err)
	}
	if tempStore != 2 {
		t.Errorf("temp_store = %d, want 2 (memory)", tempStore)
	}

	marker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("readStorageEncryptionMarker() error: %v", err)
	}
	if !marker.exists || marker.cleanupPending {
		t.Fatalf("marker = %#v, want existing with cleanup complete", marker)
	}
	if err := verifyStorageEncryptionMarker(storage, marker); err != nil {
		t.Fatalf("verifyStorageEncryptionMarker() error: %v", err)
	}
	var users, settings int
	if err := database.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 1 AND username = ?`, kritui_db.LegacyOwnerUsername).Scan(&users); err != nil {
		t.Fatalf("count seeded user: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings WHERE id = 1`).Scan(&settings); err != nil {
		t.Fatalf("count seeded settings: %v", err)
	}
	if users != 1 || settings != 1 {
		t.Errorf("fresh users = %d, settings = %d; want 1, 1", users, settings)
	}
}

func TestStorageFormat31NoOpReservesEncryptionBoundary(t *testing.T) {
	database, _ := openStorageTestDatabase(t)
	if _, err := database.Exec(`PRAGMA user_version = 30`); err != nil {
		t.Fatalf("set version 30: %v", err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrateDatabase() from version 30: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("user_version = %d, want %d", version, len(databaseMigrations))
	}
	marker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("readStorageEncryptionMarker() error: %v", err)
	}
	if marker.exists {
		t.Fatal("schema version 31 no-op created an encryption marker")
	}
}

func TestMigrateStorageFieldCoversEveryRowID(t *testing.T) {
	database, _ := openStorageTestDatabase(t)
	ctx := context.Background()
	if _, err := database.Exec(`CREATE TABLE migration_cursor_test (value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create cursor test table: %v", err)
	}
	values := []struct {
		rowID int64
		value string
	}{
		{rowID: math.MinInt64, value: "minimum rowid value"},
		{rowID: -1, value: "negative rowid value"},
		{rowID: 0, value: "zero rowid value"},
		{rowID: 1, value: "positive rowid value"},
		{rowID: math.MaxInt64, value: "maximum rowid value"},
	}
	for _, value := range values {
		if _, err := database.Exec(`INSERT INTO migration_cursor_test (rowid, value) VALUES (?, ?)`, value.rowID, value.value); err != nil {
			t.Fatalf("insert rowid %d: %v", value.rowID, err)
		}
	}

	storage := newStorageTestCipher(t, 17)
	migration := storageMigration{
		name:      "migration_cursor_test.value",
		scope:     "migration_cursor_test.value",
		kind:      storageFieldText,
		batchSize: 2,
		selectSQL: `SELECT rowid, 7, CAST(rowid AS TEXT), value
			FROM migration_cursor_test
			WHERE rowid >= ?
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE migration_cursor_test SET value = ? WHERE rowid = ?`,
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin cursor migration: %v", err)
	}
	if err := migrateStorageField(ctx, tx, storage, migration); err != nil {
		tx.Rollback()
		t.Fatalf("migrateStorageField() error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cursor migration: %v", err)
	}

	for _, value := range values {
		var stored string
		if err := database.QueryRow(`SELECT value FROM migration_cursor_test WHERE rowid = ?`, value.rowID).Scan(&stored); err != nil {
			t.Fatalf("read rowid %d: %v", value.rowID, err)
		}
		requireEncryptedText(t, stored, value.value)
		got := decryptStorageTextForTest(t, storage, 7, migration.scope, strconv.FormatInt(value.rowID, 10), stored)
		if got != value.value {
			t.Errorf("rowid %d value = %q, want %q", value.rowID, got, value.value)
		}
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM migration_cursor_test`).Scan(&count); err != nil {
		t.Fatalf("count cursor test rows: %v", err)
	}
	if count != len(values) {
		t.Errorf("cursor test rows = %d, want %d", count, len(values))
	}
}

func TestStorageEncryptionRefusesForeignKeyOrphans(t *testing.T) {
	database, _ := openStorageTestDatabase(t)
	chatID := seedChat(t, database, "Orphan test chat", nil, nil)
	if chatID != 1 {
		t.Fatalf("seeded chat ID = %d, want 1", chatID)
	}
	if _, err := database.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys for orphan fixture: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO message_user_images
			(message_id, message_role, position, filename, media_type, width, height, data)
		VALUES (999, 'user', 0, 'orphan attachment.txt', 'text/plain', 0, 0, ?)
	`, []byte("orphan attachment bytes")); err != nil {
		t.Fatalf("insert orphan attachment: %v", err)
	}
	if _, err := database.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("restore foreign keys after orphan fixture: %v", err)
	}

	storage := newStorageTestCipher(t, 19)
	err := ensureStorageEncryption(context.Background(), database, storage, true)
	if err == nil {
		t.Fatal("ensureStorageEncryption() with orphan child error = nil")
	}
	for _, leaked := range []string{"message_user_images", "orphan attachment.txt", "orphan attachment bytes"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("foreign-key refusal leaked row content %q: %v", leaked, err)
		}
	}

	marker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read marker after refused migration: %v", err)
	}
	if marker.exists {
		t.Fatal("refused migration committed a storage encryption marker")
	}
	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&title); err != nil {
		t.Fatalf("read chat title after refused migration: %v", err)
	}
	if title != "Orphan test chat" {
		t.Errorf("chat title after refused migration = %q, want untouched plaintext", title)
	}
	var orphanCount int
	var orphanFilename string
	if err := database.QueryRow(`
		SELECT COUNT(*), MIN(filename)
		FROM message_user_images
		WHERE message_id = 999
	`).Scan(&orphanCount, &orphanFilename); err != nil {
		t.Fatalf("read orphan after refused migration: %v", err)
	}
	if orphanCount != 1 || orphanFilename != "orphan attachment.txt" {
		t.Errorf("orphan row after refused migration = %d/%q, want preserved", orphanCount, orphanFilename)
	}
}

type storageMigrationFixture struct {
	attachmentData []byte
	inboxData      []byte
	providerOutput string
}

func seedStorageMigrationFixture(t *testing.T, database *sql.DB) storageMigrationFixture {
	t.Helper()
	ctx := context.Background()
	fixture := storageMigrationFixture{
		attachmentData: []byte("legacy attachment bytes"),
		inboxData:      []byte("legacy inbox attachment bytes"),
		providerOutput: `{"type":"message","content":[{"type":"output_text","text":"provider secret output"}]}`,
	}

	statements := []string{
		`INSERT INTO users (id, username, password_hash, disabled)
			VALUES (2, 'Alice', 'pbkdf2-hash-two', 0)`,
		`INSERT INTO sessions (token_hash, user_id, expires_at)
			VALUES ('session-hash-two', 2, 4102444800)`,
		`INSERT OR REPLACE INTO settings (id, default_model, ntfy_endpoint, ntfy_topic, ntfy_api_key, llm_endpoint, llm_api_key, compaction_prompt)
			VALUES
			(1, 'plain-model', 'https://ntfy.example/topic', 'legacy-ntfy-topic', 'legacy-ntfy-key', 'https://llm.example/v1', 'legacy-llm-key', 'Legacy compaction prompt'),
			(2, 'plain-model-two', '   ', '   ', '   ', 'https://llm.example/two', '   ', '   ')`,
		`INSERT INTO prompt_appends (user_id, id, position, name, text, enabled_by_default)
			VALUES (1, 'append-1', 0, 'Legacy append name', 'Legacy append text', 0)`,
		`INSERT INTO mcp_servers (user_id, id, position, name, url, authorization_token)
			VALUES
			(1, 'mcp-1', 0, 'Legacy MCP name', 'https://mcp.example/secret', 'legacy-mcp-token'),
			(1, 'mcp-2', 1, 'Second MCP name', 'https://mcp.example/two', NULL),
			(1, 'mcp-3', 2, 'Blank MCP name', 'https://mcp.example/three', '   ')`,
		`INSERT INTO chats (id, user_id, title, created_at, updated_at, pinned)
			VALUES
			(1, 1, 'Legacy chat title', '2020-01-01T00:00:00.000Z', '2020-02-02T03:04:05.000Z', 0),
			(2, 2, '', '2020-01-03T00:00:00.000Z', '2020-03-03T03:04:05.000Z', 0)`,
		`INSERT INTO messages (id, chat_id, position, role, content, model, total_tokens, cost, tool_call_id, undo_sequence, created_at)
			VALUES
			(10, 1, 0, 'user', 'legacy user content', 'm', 1, 0.1, NULL, NULL, '2020-01-01T00:00:01.000Z'),
			(11, 1, 1, 'assistant', 'legacy assistant content', 'm', 2, 0.2, NULL, NULL, '2020-01-01T00:00:02.000Z'),
			(12, 1, 2, 'user', 'hidden user content', NULL, NULL, NULL, NULL, 1, '2020-01-01T00:00:03.000Z'),
			(13, 1, 3, 'assistant', 'hidden assistant content', NULL, NULL, NULL, NULL, 1, '2020-01-01T00:00:04.000Z'),
			(14, 1, 4, 'tool', 'legacy tool result', NULL, NULL, NULL, 'call-1', NULL, '2020-01-01T00:00:05.000Z'),
			(15, 2, 0, 'user', 'second chat content', NULL, NULL, NULL, NULL, NULL, '2020-01-03T00:00:01.000Z')`,
		`INSERT INTO message_tool_calls (message_id, message_role, position, call_id, call_type, function_name, arguments)
			VALUES (11, 'assistant', 0, 'call-1', 'function', 'lookup', '{"city":"Legacy secret city"}')`,
		`INSERT INTO message_prompt_appends (message_id, message_role, position, text)
			VALUES (10, 'user', 0, 'legacy append snapshot')`,
		`INSERT INTO message_provider_outputs (message_id, message_role, position, payload)
			VALUES (11, 'assistant', 0, ?)`,
		`INSERT INTO message_user_images (message_id, message_role, position, filename, media_type, width, height, data)
			VALUES (10, 'user', 0, 'legacy secret file.pdf', 'application/pdf', 0, 0, ?)`,
		`INSERT INTO inbox_items (id, chat_id, submission_id, delivery, model, status, message_id, content, created_at)
			VALUES
			(20, 1, 'sub-pending', 'steer', 'm', 'pending', NULL, 'pending inbox secret', '2020-01-01T00:00:06.000Z'),
			(21, 1, 'sub-delivered', 'queue', 'm', 'delivered', 10, 'delivered inbox secret', '2020-01-01T00:00:07.000Z'),
			(22, 1, 'sub-cancelled', 'queue', 'm', 'cancelled', NULL, 'cancelled inbox secret', '2020-01-01T00:00:08.000Z')`,
		`INSERT INTO inbox_prompt_append_texts (inbox_id, position, text)
			VALUES (20, 0, 'pending append text'), (21, 0, 'delivered append text')`,
		`INSERT INTO inbox_attachments (inbox_id, position, filename, media_type, width, height, data)
			VALUES
			(20, 0, 'pending secret file.txt', 'text/plain', 0, 0, ?),
			(21, 0, 'delivered secret file.txt', 'text/plain', 0, 0, ?)`,
	}
	for index, statement := range statements {
		var args []any
		switch index {
		case 9:
			args = []any{fixture.providerOutput}
		case 10:
			args = []any{fixture.attachmentData}
		case 13:
			args = []any{fixture.inboxData, fixture.inboxData}
		}
		if _, err := database.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("seed storage migration statement %d: %v", index, err)
		}
	}
	// Message inserts fire the recency trigger, so restore deterministic chat
	// timestamps after seeding to observe whether encryption preserves them.
	if _, err := database.ExecContext(ctx, `
		UPDATE chats SET updated_at = '2020-02-02T03:04:05.000Z' WHERE id = 1;
		UPDATE chats SET updated_at = '2020-03-03T03:04:05.000Z' WHERE id = 2;
	`); err != nil {
		t.Fatalf("reset chat timestamps: %v", err)
	}
	return fixture
}

func TestStorageEncryptionMigratesLegacyPlaintext(t *testing.T) {
	database, path := openStorageTestDatabase(t)
	fixture := seedStorageMigrationFixture(t, database)
	storage := newStorageTestCipher(t, 7)

	if err := ensureStorageEncryption(context.Background(), database, storage, true); err != nil {
		t.Fatalf("ensureStorageEncryption() error: %v", err)
	}

	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&title); err != nil {
		t.Fatalf("read chat title: %v", err)
	}
	requireEncryptedText(t, title, "Legacy chat title")
	if got := decryptStorageTextForTest(t, storage, 1, "chats.title", "1", title); got != "Legacy chat title" {
		t.Errorf("decrypted title = %q", got)
	}
	var emptyTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 2`).Scan(&emptyTitle); err != nil {
		t.Fatalf("read empty title: %v", err)
	}
	if emptyTitle != "" {
		t.Errorf("empty chat sentinel = %q, want empty plaintext", emptyTitle)
	}

	contents := map[int64]struct {
		owner   int64
		content string
	}{
		10: {owner: 1, content: "legacy user content"},
		11: {owner: 1, content: "legacy assistant content"},
		12: {owner: 1, content: "hidden user content"},
		13: {owner: 1, content: "hidden assistant content"},
		14: {owner: 1, content: "legacy tool result"},
		15: {owner: 2, content: "second chat content"},
	}
	for id, expected := range contents {
		var stored string
		if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("read message %d content: %v", id, err)
		}
		requireEncryptedText(t, stored, expected.content)
		got := decryptStorageTextForTest(t, storage, expected.owner, "messages.content", strconv.FormatInt(id, 10), stored)
		if got != expected.content {
			t.Errorf("message %d content = %q, want %q", id, got, expected.content)
		}
	}

	var arguments string
	if err := database.QueryRow(`SELECT arguments FROM message_tool_calls WHERE message_id = 11 AND position = 0`).Scan(&arguments); err != nil {
		t.Fatalf("read tool arguments: %v", err)
	}
	requireEncryptedText(t, arguments, `{"city":"Legacy secret city"}`)
	if got := decryptStorageTextForTest(t, storage, 1, "message_tool_calls.arguments", "11:0", arguments); got != `{"city":"Legacy secret city"}` {
		t.Errorf("tool arguments = %q", got)
	}

	var appendText string
	if err := database.QueryRow(`SELECT text FROM message_prompt_appends WHERE message_id = 10 AND position = 0`).Scan(&appendText); err != nil {
		t.Fatalf("read message append text: %v", err)
	}
	requireEncryptedText(t, appendText, "legacy append snapshot")
	if got := decryptStorageTextForTest(t, storage, 1, "message_prompt_appends.text", "10:0", appendText); got != "legacy append snapshot" {
		t.Errorf("message append text = %q", got)
	}

	var filename string
	var data []byte
	if err := database.QueryRow(`SELECT filename, data FROM message_user_images WHERE message_id = 10 AND position = 0`).Scan(&filename, &data); err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	requireEncryptedText(t, filename, "legacy secret file.pdf")
	if got := decryptStorageTextForTest(t, storage, 1, "message_user_images.filename", "10:0", filename); got != "legacy secret file.pdf" {
		t.Errorf("attachment filename = %q", got)
	}
	if bytes.Equal(data, fixture.attachmentData) {
		t.Fatal("attachment data was not encrypted")
	}
	if got := decryptStorageBytesForTest(t, storage, 1, "message_user_images.data", "10:0", data); !bytes.Equal(got, fixture.attachmentData) {
		t.Errorf("attachment data = %q, want %q", got, fixture.attachmentData)
	}

	var payload string
	if err := database.QueryRow(`SELECT payload FROM message_provider_outputs WHERE message_id = 11 AND position = 0`).Scan(&payload); err != nil {
		t.Fatalf("read provider output: %v", err)
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &wrapper); err != nil {
		t.Fatalf("decode provider wrapper: %v", err)
	}
	if len(wrapper) != 1 {
		t.Fatalf("provider wrapper fields = %d, want 1: %s", len(wrapper), payload)
	}
	rawEnvelope, ok := wrapper["encrypted"]
	if !ok {
		t.Fatalf("provider wrapper missing encrypted field: %s", payload)
	}
	var envelope string
	if err := json.Unmarshal(rawEnvelope, &envelope); err != nil {
		t.Fatalf("decode provider envelope: %v", err)
	}
	requireEncryptedText(t, envelope, fixture.providerOutput)
	if got := decryptStorageTextForTest(t, storage, 1, "message_provider_outputs.payload", "11:0", envelope); got != fixture.providerOutput {
		t.Errorf("provider output = %q, want %q", got, fixture.providerOutput)
	}

	inboxContents := map[int64]string{
		20: "pending inbox secret",
		21: "delivered inbox secret",
		22: "cancelled inbox secret",
	}
	for id, expected := range inboxContents {
		var stored string
		if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("read inbox %d content: %v", id, err)
		}
		requireEncryptedText(t, stored, expected)
		if got := decryptStorageTextForTest(t, storage, 1, "inbox_items.content", strconv.FormatInt(id, 10), stored); got != expected {
			t.Errorf("inbox %d content = %q, want %q", id, got, expected)
		}
	}
	for inboxID, expected := range map[int64]string{20: "pending append text", 21: "delivered append text"} {
		var stored string
		if err := database.QueryRow(`SELECT text FROM inbox_prompt_append_texts WHERE inbox_id = ? AND position = 0`, inboxID).Scan(&stored); err != nil {
			t.Fatalf("read inbox append %d: %v", inboxID, err)
		}
		requireEncryptedText(t, stored, expected)
		if got := decryptStorageTextForTest(t, storage, 1, "inbox_prompt_append_texts.text", strconv.FormatInt(inboxID, 10)+":0", stored); got != expected {
			t.Errorf("inbox append %d = %q, want %q", inboxID, got, expected)
		}
	}
	for inboxID, expected := range map[int64]string{20: "pending secret file.txt", 21: "delivered secret file.txt"} {
		var storedFilename string
		var storedData []byte
		if err := database.QueryRow(`SELECT filename, data FROM inbox_attachments WHERE inbox_id = ? AND position = 0`, inboxID).Scan(&storedFilename, &storedData); err != nil {
			t.Fatalf("read inbox attachment %d: %v", inboxID, err)
		}
		requireEncryptedText(t, storedFilename, expected)
		if got := decryptStorageTextForTest(t, storage, 1, "inbox_attachments.filename", strconv.FormatInt(inboxID, 10)+":0", storedFilename); got != expected {
			t.Errorf("inbox attachment %d filename = %q, want %q", inboxID, got, expected)
		}
		if got := decryptStorageBytesForTest(t, storage, 1, "inbox_attachments.data", strconv.FormatInt(inboxID, 10)+":0", storedData); !bytes.Equal(got, fixture.inboxData) {
			t.Errorf("inbox attachment %d data = %q, want %q", inboxID, got, fixture.inboxData)
		}
	}

	for _, testCase := range []struct {
		owner int64
		scope string
		rowID string
		value string
	}{
		{1, "settings.llm_api_key", "1", "legacy-llm-key"},
		{1, "settings.ntfy_api_key", "1", "legacy-ntfy-key"},
		{1, "settings.llm_endpoint", "1", "https://llm.example/v1"},
		{1, "settings.ntfy_endpoint", "1", "https://ntfy.example/topic"},
		{1, "settings.ntfy_topic", "1", "legacy-ntfy-topic"},
		{1, "settings.compaction_prompt", "1", "Legacy compaction prompt"},
		{2, "settings.llm_endpoint", "2", "https://llm.example/two"},
	} {
		var stored string
		query := `SELECT ` + strings.TrimPrefix(testCase.scope, "settings.") + ` FROM settings WHERE id = ?`
		if err := database.QueryRow(query, testCase.owner).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", testCase.scope, err)
		}
		requireEncryptedText(t, stored, testCase.value)
		if got := decryptStorageTextForTest(t, storage, testCase.owner, testCase.scope, testCase.rowID, stored); got != testCase.value {
			t.Errorf("%s = %q, want %q", testCase.scope, got, testCase.value)
		}
	}
	for _, column := range []string{"llm_api_key", "ntfy_api_key", "ntfy_endpoint", "ntfy_topic", "compaction_prompt"} {
		var blank sql.NullString
		if err := database.QueryRow(`SELECT ` + column + ` FROM settings WHERE id = 2`).Scan(&blank); err != nil {
			t.Fatalf("read blank settings %s: %v", column, err)
		}
		if blank.Valid {
			t.Errorf("settings id 2 %s = %q, want NULL for blank legacy secret", column, blank.String)
		}
	}
	ntfySettings, err := kritui_db.GetNtfySettings(storageTestContext(storage, 2), database)
	if err != nil {
		t.Fatalf("GetNtfySettings() error: %v", err)
	}
	if ntfySettings.APIKeyConfigured {
		t.Error("blank ntfy API key migrated as configured")
	}
	llmSettings, err := kritui_db.GetLLMSettings(storageTestContext(storage, 2), database)
	if err != nil {
		t.Fatalf("GetLLMSettings() error: %v", err)
	}
	if llmSettings.APIKeyConfigured {
		t.Error("blank LLM API key migrated as configured")
	}
	compactionPrompt, err := kritui_db.GetCompactionPrompt(storageTestContext(storage, 1), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if compactionPrompt != "Legacy compaction prompt" {
		t.Errorf("GetCompactionPrompt() = %q, want migrated custom prompt", compactionPrompt)
	}
	blankCompactionPrompt, err := kritui_db.GetCompactionPrompt(storageTestContext(storage, 2), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt(blank) error: %v", err)
	}
	if blankCompactionPrompt != llm.DefaultCompactionPrompt() {
		t.Errorf("GetCompactionPrompt(blank) = %q, want default", blankCompactionPrompt)
	}

	for _, testCase := range []struct {
		scope string
		rowID string
		value string
	}{
		{"mcp_servers.name", "mcp-1", "Legacy MCP name"},
		{"mcp_servers.url", "mcp-1", "https://mcp.example/secret"},
		{"mcp_servers.authorization_token", "mcp-1", "legacy-mcp-token"},
		{"mcp_servers.name", "mcp-2", "Second MCP name"},
		{"mcp_servers.url", "mcp-2", "https://mcp.example/two"},
		{"mcp_servers.name", "mcp-3", "Blank MCP name"},
		{"mcp_servers.url", "mcp-3", "https://mcp.example/three"},
	} {
		var stored string
		query := `SELECT ` + strings.TrimPrefix(testCase.scope, "mcp_servers.") + ` FROM mcp_servers WHERE id = ?`
		if err := database.QueryRow(query, testCase.rowID).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", testCase.scope, err)
		}
		requireEncryptedText(t, stored, testCase.value)
		if got := decryptStorageTextForTest(t, storage, 1, testCase.scope, testCase.rowID, stored); got != testCase.value {
			t.Errorf("%s = %q, want %q", testCase.scope, got, testCase.value)
		}
	}
	var nullToken sql.NullString
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = 'mcp-2'`).Scan(&nullToken); err != nil {
		t.Fatalf("read null mcp token: %v", err)
	}
	if nullToken.Valid {
		t.Errorf("mcp-2 authorization_token = %q, want NULL", nullToken.String)
	}
	var blankToken sql.NullString
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = 'mcp-3'`).Scan(&blankToken); err != nil {
		t.Fatalf("read blank mcp token: %v", err)
	}
	if blankToken.Valid {
		t.Errorf("mcp-3 authorization_token = %q, want NULL for blank legacy token", blankToken.String)
	}

	var appendName, appendDefinition string
	if err := database.QueryRow(`SELECT name, text FROM prompt_appends WHERE id = 'append-1'`).Scan(&appendName, &appendDefinition); err != nil {
		t.Fatalf("read prompt append: %v", err)
	}
	requireEncryptedText(t, appendName, "Legacy append name")
	requireEncryptedText(t, appendDefinition, "Legacy append text")
	if got := decryptStorageTextForTest(t, storage, 1, "prompt_appends.name", "append-1", appendName); got != "Legacy append name" {
		t.Errorf("prompt append name = %q", got)
	}
	if got := decryptStorageTextForTest(t, storage, 1, "prompt_appends.text", "append-1", appendDefinition); got != "Legacy append text" {
		t.Errorf("prompt append text = %q", got)
	}

	var username, passwordHash string
	if err := database.QueryRow(`SELECT username, password_hash FROM users WHERE id = 2`).Scan(&username, &passwordHash); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if username != "Alice" || passwordHash != "pbkdf2-hash-two" {
		t.Errorf("account = %q/%q, want preserved plaintext metadata", username, passwordHash)
	}
	var sessionHash string
	var expiresAt int64
	if err := database.QueryRow(`SELECT token_hash, expires_at FROM sessions WHERE user_id = 2`).Scan(&sessionHash, &expiresAt); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sessionHash != "session-hash-two" || expiresAt != 4102444800 {
		t.Errorf("session = %q/%d, want preserved", sessionHash, expiresAt)
	}

	var updatedAt string
	if err := database.QueryRow(`SELECT updated_at FROM chats WHERE id = 1`).Scan(&updatedAt); err != nil {
		t.Fatalf("read chat timestamp: %v", err)
	}
	if updatedAt != "2020-02-02T03:04:05.000Z" {
		t.Errorf("chat updated_at = %q, want original timestamp", updatedAt)
	}
	var position int
	var role, createdAt string
	if err := database.QueryRow(`SELECT position, role, created_at FROM messages WHERE id = 14`).Scan(&position, &role, &createdAt); err != nil {
		t.Fatalf("read preserved message metadata: %v", err)
	}
	if position != 4 || role != "tool" || createdAt != "2020-01-01T00:00:05.000Z" {
		t.Errorf("message metadata = %d/%q/%q, want preserved", position, role, createdAt)
	}

	marker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("readStorageEncryptionMarker() error: %v", err)
	}
	if !marker.exists || marker.cleanupPending {
		t.Fatalf("marker = %#v, want existing with cleanup complete", marker)
	}
	if err := verifyStorageEncryptionMarker(storage, marker); err != nil {
		t.Fatalf("verifyStorageEncryptionMarker() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	for _, sentinel := range []string{
		"Legacy chat title",
		"legacy user content",
		"hidden assistant content",
		"legacy tool result",
		"Legacy secret city",
		"legacy append snapshot",
		"legacy secret file.pdf",
		"legacy attachment bytes",
		"provider secret output",
		"pending inbox secret",
		"delivered inbox secret",
		"pending append text",
		"pending secret file.txt",
		"legacy inbox attachment bytes",
		"legacy-llm-key",
		"legacy-ntfy-topic",
		"Legacy compaction prompt",
		"Legacy MCP name",
		"legacy-mcp-token",
		"Legacy append name",
	} {
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Errorf("database file still contains plaintext sentinel %q after VACUUM", sentinel)
		}
	}
	if _, err := os.Stat(path + "-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("journal file remains after migration: %v", err)
	}
}

func TestStorageEncryptionRepeatStartupIsIdempotent(t *testing.T) {
	database, _ := openStorageTestDatabase(t)
	seedStorageMigrationFixture(t, database)
	storage := newStorageTestCipher(t, 9)

	if err := ensureStorageEncryption(context.Background(), database, storage, true); err != nil {
		t.Fatalf("first ensureStorageEncryption() error: %v", err)
	}
	var firstTitle, firstContent, firstSetting string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&firstTitle); err != nil {
		t.Fatalf("read first title: %v", err)
	}
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = 10`).Scan(&firstContent); err != nil {
		t.Fatalf("read first content: %v", err)
	}
	if err := database.QueryRow(`SELECT llm_api_key FROM settings WHERE id = 1`).Scan(&firstSetting); err != nil {
		t.Fatalf("read first setting: %v", err)
	}
	firstMarker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read first marker: %v", err)
	}

	// An in-development encrypted database may still carry schema version 30.
	// The reserved version-31 no-op and version-32 metadata migration must
	// upgrade it after key verification without re-encrypting any value.
	if _, err := database.Exec(`PRAGMA user_version = 30`); err != nil {
		t.Fatalf("set in-development version 30: %v", err)
	}
	if err := ensureStorageEncryption(context.Background(), database, storage, true); err != nil {
		t.Fatalf("second ensureStorageEncryption() error: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read upgraded version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Errorf("user_version after encrypted upgrade = %d, want %d", version, len(databaseMigrations))
	}
	var secondTitle, secondContent, secondSetting string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&secondTitle); err != nil {
		t.Fatalf("read second title: %v", err)
	}
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = 10`).Scan(&secondContent); err != nil {
		t.Fatalf("read second content: %v", err)
	}
	if err := database.QueryRow(`SELECT llm_api_key FROM settings WHERE id = 1`).Scan(&secondSetting); err != nil {
		t.Fatalf("read second setting: %v", err)
	}
	secondMarker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read second marker: %v", err)
	}

	if firstTitle != secondTitle || firstContent != secondContent || firstSetting != secondSetting {
		t.Error("repeat startup re-encrypted stored values")
	}
	if !bytes.Equal(firstMarker.keyCheck, secondMarker.keyCheck) || secondMarker.cleanupPending {
		t.Error("repeat startup replaced the key check or left cleanup pending")
	}
}

func TestStorageEncryptionWrongKeyDoesNotMutate(t *testing.T) {
	database, _ := openStorageTestDatabase(t)
	seedStorageMigrationFixture(t, database)
	correct := newStorageTestCipher(t, 11)
	if err := ensureStorageEncryption(context.Background(), database, correct, true); err != nil {
		t.Fatalf("ensureStorageEncryption() error: %v", err)
	}
	if _, err := database.Exec(`UPDATE storage_encryption SET cleanup_pending = 1 WHERE id = 1`); err != nil {
		t.Fatalf("set cleanup pending: %v", err)
	}
	var beforeTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&beforeTitle); err != nil {
		t.Fatalf("read title before wrong key: %v", err)
	}
	beforeMarker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read marker before wrong key: %v", err)
	}

	wrong := newStorageTestCipher(t, 12)
	wrongEncoded := base64.StdEncoding.EncodeToString(storageTestKey(12))
	err = ensureStorageEncryption(context.Background(), database, wrong, true)
	if err == nil {
		t.Fatal("ensureStorageEncryption() with wrong key error = nil")
	}
	if strings.Contains(err.Error(), wrongEncoded) {
		t.Fatal("wrong-key error leaked the key")
	}
	var afterTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&afterTitle); err != nil {
		t.Fatalf("read title after wrong key: %v", err)
	}
	afterMarker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read marker after wrong key: %v", err)
	}
	if beforeTitle != afterTitle {
		t.Error("wrong key changed encrypted data")
	}
	if !bytes.Equal(beforeMarker.keyCheck, afterMarker.keyCheck) || !afterMarker.cleanupPending {
		t.Error("wrong key changed the marker or cleared cleanup_pending")
	}
}

func TestStorageEncryptionCleanupRetry(t *testing.T) {
	database, path := openStorageTestDatabase(t)
	seedStorageMigrationFixture(t, database)
	storage := newStorageTestCipher(t, 13)
	if err := ensureStorageEncryption(context.Background(), database, storage, true); err != nil {
		t.Fatalf("ensureStorageEncryption() error: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title) VALUES (99, 1, 'retry scrub secret');
		DELETE FROM chats WHERE id = 99;
		UPDATE storage_encryption SET cleanup_pending = 1 WHERE id = 1;
	`); err != nil {
		t.Fatalf("seed retry cleanup state: %v", err)
	}

	if err := ensureStorageEncryption(context.Background(), database, storage, true); err != nil {
		t.Fatalf("retry ensureStorageEncryption() error: %v", err)
	}
	marker, err := readStorageEncryptionMarker(context.Background(), database)
	if err != nil {
		t.Fatalf("read marker after retry: %v", err)
	}
	if marker.cleanupPending {
		t.Error("cleanup_pending remains set after successful retry")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	if bytes.Contains(raw, []byte("retry scrub secret")) {
		t.Error("retry VACUUM left plaintext in the database file")
	}
}
