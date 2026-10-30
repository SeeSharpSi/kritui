package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"unicode"

	kritui_db "seesharpsi/kritui/db"
)

const (
	// storageEncryptionKeyEnvironment names the mandatory key variable. The
	// value is standard base64 for exactly 32 bytes and is never logged.
	storageEncryptionKeyEnvironment = "KRITUI_ENCRYPTION_KEY"

	storageEncryptionCheckScope   = "storage_encryption.key_check"
	storageEncryptionCheckOwnerID = int64(0)
	storageEncryptionCheckRowID   = "1"
	storageEncryptionCheckValue   = "kritui storage encryption key check v1"

	storageEncryptionMigrationBatch = 32
	storageEncryptionBinaryBatch    = 1
)

// storageEncryptionMarker is the authenticated key check persisted beside the
// encrypted data. Its presence means the database has completed the one-way
// plaintext migration and must never be migrated again.
type storageEncryptionMarker struct {
	exists         bool
	keyCheck       []byte
	cleanupPending bool
}

type storageFieldKind uint8

const (
	storageFieldText storageFieldKind = iota
	storageFieldBytes
	storageFieldProviderOutput
)

// storageMigration describes one sensitive column. selectSQL returns
// (rowid, owner, authenticated row ID, value) and is paginated by rowid so the
// migration holds at most one bounded batch in memory.
type storageMigration struct {
	name               string
	scope              string
	kind               storageFieldKind
	batchSize          int
	normalizeBlankNull bool
	selectSQL          string
	updateSQL          string
}

var storageMigrations = []storageMigration{
	{
		name:  "chats.title",
		scope: "chats.title",
		kind:  storageFieldText,
		selectSQL: `SELECT rowid, COALESCE(user_id, 1), CAST(id AS TEXT), title
			FROM chats
			WHERE rowid >= ? AND title <> ''
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE chats SET title = ? WHERE rowid = ?`,
	},
	{
		name:  "messages.content",
		scope: "messages.content",
		kind:  storageFieldText,
		selectSQL: `SELECT m.rowid, COALESCE(c.user_id, 1), CAST(m.id AS TEXT), m.content
			FROM messages AS m
			JOIN chats AS c ON c.id = m.chat_id
			WHERE m.rowid >= ?
			ORDER BY m.rowid
			LIMIT ?`,
		updateSQL: `UPDATE messages SET content = ? WHERE rowid = ?`,
	},
	{
		name:  "message_tool_calls.arguments",
		scope: "message_tool_calls.arguments",
		kind:  storageFieldText,
		selectSQL: `SELECT t.rowid, COALESCE(c.user_id, 1), printf('%d:%d', t.message_id, t.position), t.arguments
			FROM message_tool_calls AS t
			JOIN messages AS m ON m.id = t.message_id
			JOIN chats AS c ON c.id = m.chat_id
			WHERE t.rowid >= ?
			ORDER BY t.rowid
			LIMIT ?`,
		updateSQL: `UPDATE message_tool_calls SET arguments = ? WHERE rowid = ?`,
	},
	{
		name:  "message_prompt_appends.text",
		scope: "message_prompt_appends.text",
		kind:  storageFieldText,
		selectSQL: `SELECT p.rowid, COALESCE(c.user_id, 1), printf('%d:%d', p.message_id, p.position), p.text
			FROM message_prompt_appends AS p
			JOIN messages AS m ON m.id = p.message_id
			JOIN chats AS c ON c.id = m.chat_id
			WHERE p.rowid >= ?
			ORDER BY p.rowid
			LIMIT ?`,
		updateSQL: `UPDATE message_prompt_appends SET text = ? WHERE rowid = ?`,
	},
	{
		name:  "message_user_images.filename",
		scope: "message_user_images.filename",
		kind:  storageFieldText,
		selectSQL: `SELECT i.rowid, COALESCE(c.user_id, 1), printf('%d:%d', i.message_id, i.position), i.filename
			FROM message_user_images AS i
			JOIN messages AS m ON m.id = i.message_id
			JOIN chats AS c ON c.id = m.chat_id
			WHERE i.rowid >= ?
			ORDER BY i.rowid
			LIMIT ?`,
		updateSQL: `UPDATE message_user_images SET filename = ? WHERE rowid = ?`,
	},
	{
		name:      "message_user_images.data",
		scope:     "message_user_images.data",
		kind:      storageFieldBytes,
		batchSize: storageEncryptionBinaryBatch,
		selectSQL: `SELECT i.rowid, COALESCE(c.user_id, 1), printf('%d:%d', i.message_id, i.position), i.data
			FROM message_user_images AS i
			JOIN messages AS m ON m.id = i.message_id
			JOIN chats AS c ON c.id = m.chat_id
			WHERE i.rowid >= ?
			ORDER BY i.rowid
			LIMIT ?`,
		updateSQL: `UPDATE message_user_images SET data = ? WHERE rowid = ?`,
	},
	{
		name:  "message_provider_outputs.payload",
		scope: "message_provider_outputs.payload",
		kind:  storageFieldProviderOutput,
		selectSQL: `SELECT o.rowid, COALESCE(c.user_id, 1), printf('%d:%d', o.message_id, o.position), o.payload
			FROM message_provider_outputs AS o
			JOIN messages AS m ON m.id = o.message_id
			JOIN chats AS c ON c.id = m.chat_id
			WHERE o.rowid >= ?
			ORDER BY o.rowid
			LIMIT ?`,
		updateSQL: `UPDATE message_provider_outputs SET payload = ? WHERE rowid = ?`,
	},
	{
		name:  "inbox_items.content",
		scope: "inbox_items.content",
		kind:  storageFieldText,
		selectSQL: `SELECT i.rowid, COALESCE(c.user_id, 1), CAST(i.id AS TEXT), i.content
			FROM inbox_items AS i
			JOIN chats AS c ON c.id = i.chat_id
			WHERE i.rowid >= ?
			ORDER BY i.rowid
			LIMIT ?`,
		updateSQL: `UPDATE inbox_items SET content = ? WHERE rowid = ?`,
	},
	{
		name:  "inbox_prompt_append_texts.text",
		scope: "inbox_prompt_append_texts.text",
		kind:  storageFieldText,
		selectSQL: `SELECT p.rowid, COALESCE(c.user_id, 1), printf('%d:%d', p.inbox_id, p.position), p.text
			FROM inbox_prompt_append_texts AS p
			JOIN inbox_items AS i ON i.id = p.inbox_id
			JOIN chats AS c ON c.id = i.chat_id
			WHERE p.rowid >= ?
			ORDER BY p.rowid
			LIMIT ?`,
		updateSQL: `UPDATE inbox_prompt_append_texts SET text = ? WHERE rowid = ?`,
	},
	{
		name:  "inbox_attachments.filename",
		scope: "inbox_attachments.filename",
		kind:  storageFieldText,
		selectSQL: `SELECT a.rowid, COALESCE(c.user_id, 1), printf('%d:%d', a.inbox_id, a.position), a.filename
			FROM inbox_attachments AS a
			JOIN inbox_items AS i ON i.id = a.inbox_id
			JOIN chats AS c ON c.id = i.chat_id
			WHERE a.rowid >= ?
			ORDER BY a.rowid
			LIMIT ?`,
		updateSQL: `UPDATE inbox_attachments SET filename = ? WHERE rowid = ?`,
	},
	{
		name:      "inbox_attachments.data",
		scope:     "inbox_attachments.data",
		kind:      storageFieldBytes,
		batchSize: storageEncryptionBinaryBatch,
		selectSQL: `SELECT a.rowid, COALESCE(c.user_id, 1), printf('%d:%d', a.inbox_id, a.position), a.data
			FROM inbox_attachments AS a
			JOIN inbox_items AS i ON i.id = a.inbox_id
			JOIN chats AS c ON c.id = i.chat_id
			WHERE a.rowid >= ?
			ORDER BY a.rowid
			LIMIT ?`,
		updateSQL: `UPDATE inbox_attachments SET data = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.llm_api_key",
		scope:              "settings.llm_api_key",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), llm_api_key
			FROM settings
			WHERE rowid >= ? AND llm_api_key IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET llm_api_key = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.ntfy_api_key",
		scope:              "settings.ntfy_api_key",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), ntfy_api_key
			FROM settings
			WHERE rowid >= ? AND ntfy_api_key IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET ntfy_api_key = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.llm_endpoint",
		scope:              "settings.llm_endpoint",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), llm_endpoint
			FROM settings
			WHERE rowid >= ? AND llm_endpoint IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET llm_endpoint = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.ntfy_endpoint",
		scope:              "settings.ntfy_endpoint",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), ntfy_endpoint
			FROM settings
			WHERE rowid >= ? AND ntfy_endpoint IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET ntfy_endpoint = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.ntfy_topic",
		scope:              "settings.ntfy_topic",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), ntfy_topic
			FROM settings
			WHERE rowid >= ? AND ntfy_topic IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET ntfy_topic = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.system_prompt",
		scope:              "settings.system_prompt",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), system_prompt
			FROM settings
			WHERE rowid >= ? AND system_prompt IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET system_prompt = ? WHERE rowid = ?`,
	},
	{
		name:               "settings.compaction_prompt",
		scope:              "settings.compaction_prompt",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, id, CAST(id AS TEXT), compaction_prompt
			FROM settings
			WHERE rowid >= ? AND compaction_prompt IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE settings SET compaction_prompt = ? WHERE rowid = ?`,
	},
	{
		name:  "mcp_servers.name",
		scope: "mcp_servers.name",
		kind:  storageFieldText,
		selectSQL: `SELECT rowid, user_id, id, name
			FROM mcp_servers
			WHERE rowid >= ?
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE mcp_servers SET name = ? WHERE rowid = ?`,
	},
	{
		name:  "mcp_servers.url",
		scope: "mcp_servers.url",
		kind:  storageFieldText,
		selectSQL: `SELECT rowid, user_id, id, url
			FROM mcp_servers
			WHERE rowid >= ?
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE mcp_servers SET url = ? WHERE rowid = ?`,
	},
	{
		name:               "mcp_servers.authorization_token",
		scope:              "mcp_servers.authorization_token",
		kind:               storageFieldText,
		normalizeBlankNull: true,
		selectSQL: `SELECT rowid, user_id, id, authorization_token
			FROM mcp_servers
			WHERE rowid >= ? AND authorization_token IS NOT NULL
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE mcp_servers SET authorization_token = ? WHERE rowid = ?`,
	},
	{
		name:  "prompt_appends.name",
		scope: "prompt_appends.name",
		kind:  storageFieldText,
		selectSQL: `SELECT rowid, user_id, id, name
			FROM prompt_appends
			WHERE rowid >= ?
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE prompt_appends SET name = ? WHERE rowid = ?`,
	},
	{
		name:  "prompt_appends.text",
		scope: "prompt_appends.text",
		kind:  storageFieldText,
		selectSQL: `SELECT rowid, user_id, id, text
			FROM prompt_appends
			WHERE rowid >= ?
			ORDER BY rowid
			LIMIT ?`,
		updateSQL: `UPDATE prompt_appends SET text = ? WHERE rowid = ?`,
	},
}

// storageEncryptionKeyFromEnvironment reads and strictly validates the
// mandatory key before the database is opened. Leading and trailing whitespace
// is trimmed deliberately so an environment file may end a line normally, but
// embedded whitespace or any non-standard base64 is rejected. Errors never
// include the supplied value.
func storageEncryptionKeyFromEnvironment() ([]byte, error) {
	value, ok := os.LookupEnv(storageEncryptionKeyEnvironment)
	if !ok {
		return nil, fmt.Errorf("%s is required and must be standard base64 encoding of exactly %d bytes", storageEncryptionKeyEnvironment, kritui_db.StorageKeySize)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("%s is required and must be standard base64 encoding of exactly %d bytes", storageEncryptionKeyEnvironment, kritui_db.StorageKeySize)
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			return nil, fmt.Errorf("%s must not contain embedded whitespace", storageEncryptionKeyEnvironment)
		}
	}
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != kritui_db.StorageKeySize {
		return nil, fmt.Errorf("%s must be standard base64 encoding of exactly %d bytes", storageEncryptionKeyEnvironment, kritui_db.StorageKeySize)
	}
	return key, nil
}

// storageEncryptionCipherFromEnvironment parses the key, builds the cipher, and
// clears the temporary key buffer. The key itself is never logged or returned
// in an error.
func storageEncryptionCipherFromEnvironment() (*kritui_db.StorageCipher, error) {
	key, err := storageEncryptionKeyFromEnvironment()
	if err != nil {
		return nil, err
	}
	defer clear(key)
	storage, err := kritui_db.NewStorageCipher(key)
	if err != nil {
		return nil, fmt.Errorf("configure storage encryption: %w", err)
	}
	return storage, nil
}

// withStorageEncryption injects the process cipher into every production
// request before authentication runs. Worker contexts derived with
// context.WithoutCancel preserve this value.
func withStorageEncryption(storage *kritui_db.StorageCipher, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := kritui_db.WithStorageCipher(r.Context(), storage)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// readStorageEncryptionMarker inspects the marker without changing the
// database. A missing table means a legacy or fresh database; a table without
// its single authenticated row is corrupt and refuses startup.
func readStorageEncryptionMarker(ctx context.Context, database *sql.DB) (storageEncryptionMarker, error) {
	var tableCount int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name = 'storage_encryption'
	`).Scan(&tableCount); err != nil {
		return storageEncryptionMarker{}, fmt.Errorf("inspect storage encryption marker: %w", err)
	}
	if tableCount == 0 {
		return storageEncryptionMarker{}, nil
	}

	var marker storageEncryptionMarker
	var cleanupPending int
	err := database.QueryRowContext(ctx, `
		SELECT key_check, cleanup_pending
		FROM storage_encryption
		WHERE id = 1
	`).Scan(&marker.keyCheck, &cleanupPending)
	if errors.Is(err, sql.ErrNoRows) {
		return storageEncryptionMarker{}, errors.New("storage encryption marker is incomplete")
	}
	if err != nil {
		return storageEncryptionMarker{}, fmt.Errorf("read storage encryption marker: %w", err)
	}
	if len(marker.keyCheck) == 0 {
		return storageEncryptionMarker{}, errors.New("storage encryption marker is incomplete")
	}
	marker.exists = true
	marker.cleanupPending = cleanupPending != 0
	return marker, nil
}

// verifyStorageEncryptionMarker authenticates the fixed key-check literal with
// the configured key. It performs no writes, so a wrong key cannot mutate the
// database before startup refuses.
func verifyStorageEncryptionMarker(storage *kritui_db.StorageCipher, marker storageEncryptionMarker) error {
	if storage == nil {
		return errors.New("storage encryption key is unavailable")
	}
	plaintext, err := storage.Open(marker.keyCheck, storageEncryptionCheckScope, storageEncryptionCheckOwnerID, storageEncryptionCheckRowID)
	if err != nil {
		return errors.New("storage encryption key does not match this database")
	}
	if subtle.ConstantTimeCompare(plaintext, []byte(storageEncryptionCheckValue)) != 1 {
		return errors.New("storage encryption key does not match this database")
	}
	return nil
}

// ensureStorageEncryption verifies an existing key check before any schema or
// data mutation, then initializes or migrates the schema. A database without a
// marker is legacy or fresh and receives the one-way plaintext migration. A
// marker with cleanup_pending resumes the VACUUM scrub without re-encrypting
// already encrypted values.
func ensureStorageEncryption(ctx context.Context, database *sql.DB, storage *kritui_db.StorageCipher, databaseExists bool) error {
	marker, err := readStorageEncryptionMarker(ctx, database)
	if err != nil {
		return err
	}
	if marker.exists {
		if err := verifyStorageEncryptionMarker(storage, marker); err != nil {
			return err
		}
	}

	if !databaseExists {
		if _, err := database.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("initialize database: %w", err)
		}
	}
	if err := migrateDatabase(database); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if !marker.exists {
		if err := migrateStorageEncryption(ctx, database, storage); err != nil {
			return err
		}
		marker.cleanupPending = true
	}
	if marker.cleanupPending {
		if err := scrubStorageEncryption(ctx, database); err != nil {
			return err
		}
	}
	return nil
}

// migrateStorageEncryption performs the one-way plaintext migration in a single
// transaction and records the authenticated key check with cleanup_pending=1.
// It must only run when no marker exists, so existing encrypted data is never
// re-encrypted and legacy plaintext is never interpreted as a fallback.
func migrateStorageEncryption(ctx context.Context, database *sql.DB, storage *kritui_db.StorageCipher) error {
	if storage == nil {
		return errors.New("migrate storage encryption: storage cipher unavailable")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin storage encryption migration: %w", err)
	}
	defer tx.Rollback()

	// Child descriptors derive ownership by joining their parent. Refuse the
	// whole migration if legacy writes with foreign keys disabled left orphans,
	// rather than committing a marker while skipping unowned plaintext rows.
	if err := checkStorageMigrationForeignKeys(ctx, tx); err != nil {
		return err
	}

	// Updating messages fires messages_touch_chat_after_update, which rewrites
	// chats.updated_at. Preserve the original recency values and restore them
	// before committing so migration does not reorder history.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.storage_encryption_chat_timestamps`); err != nil {
		return fmt.Errorf("reset chat timestamp backup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE storage_encryption_chat_timestamps AS
		SELECT id, updated_at FROM chats
	`); err != nil {
		return fmt.Errorf("back up chat timestamps: %w", err)
	}

	for _, migration := range storageMigrations {
		if err := migrateStorageField(ctx, tx, storage, migration); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE chats
		SET updated_at = (
			SELECT backup.updated_at
			FROM temp.storage_encryption_chat_timestamps AS backup
			WHERE backup.id = chats.id
		)
		WHERE id IN (SELECT id FROM temp.storage_encryption_chat_timestamps)
	`); err != nil {
		return fmt.Errorf("restore chat timestamps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE temp.storage_encryption_chat_timestamps`); err != nil {
		return fmt.Errorf("drop chat timestamp backup: %w", err)
	}

	keyCheck, err := storage.Seal(
		[]byte(storageEncryptionCheckValue),
		storageEncryptionCheckScope,
		storageEncryptionCheckOwnerID,
		storageEncryptionCheckRowID,
	)
	if err != nil {
		return fmt.Errorf("create storage encryption key check: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE storage_encryption (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			key_check BLOB NOT NULL CHECK (length(key_check) > 0),
			cleanup_pending INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_pending IN (0, 1)),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT
	`); err != nil {
		return fmt.Errorf("create storage encryption marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO storage_encryption (id, key_check, cleanup_pending)
		VALUES (1, ?, 1)
	`, keyCheck); err != nil {
		return fmt.Errorf("store storage encryption marker: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit storage encryption migration: %w", err)
	}
	return nil
}

func checkStorageMigrationForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check storage migration foreign keys: %w", err)
	}
	violation := false
	for rows.Next() {
		violation = true
		break
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("check storage migration foreign keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("check storage migration foreign keys: %w", err)
	}
	if violation {
		return errors.New("storage encryption migration refused: database contains foreign key violations")
	}
	return nil
}

func migrateStorageField(ctx context.Context, tx *sql.Tx, storage *kritui_db.StorageCipher, migration storageMigration) error {
	batchSize := migration.batchSize
	if batchSize <= 0 {
		batchSize = storageEncryptionMigrationBatch
	}
	type storageRow struct {
		rowID  int64
		owner  int64
		bindID string
		value  []byte
	}
	afterRowID := int64(math.MinInt64)
	for {
		rows, err := tx.QueryContext(ctx, migration.selectSQL, afterRowID, batchSize)
		if err != nil {
			return fmt.Errorf("read %s for encryption: %w", migration.name, err)
		}
		batch := make([]storageRow, 0, batchSize)
		for rows.Next() {
			var row storageRow
			if err := rows.Scan(&row.rowID, &row.owner, &row.bindID, &row.value); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s for encryption: %w", migration.name, err)
			}
			batch = append(batch, row)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close %s for encryption: %w", migration.name, err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate %s for encryption: %w", migration.name, err)
		}
		if len(batch) == 0 {
			return nil
		}
		lastRowID := batch[len(batch)-1].rowID
		for _, row := range batch {
			if migration.normalizeBlankNull && strings.TrimSpace(string(row.value)) == "" {
				if _, err := tx.ExecContext(ctx, migration.updateSQL, nil, row.rowID); err != nil {
					return fmt.Errorf("normalize blank %s row %d: %w", migration.name, row.rowID, err)
				}
				continue
			}
			value, err := sealStorageValue(ctx, storage, migration, row.owner, row.bindID, row.value)
			if err != nil {
				return fmt.Errorf("encrypt %s row %d: %w", migration.name, row.rowID, err)
			}
			if _, err := tx.ExecContext(ctx, migration.updateSQL, value, row.rowID); err != nil {
				return fmt.Errorf("store encrypted %s row %d: %w", migration.name, row.rowID, err)
			}
		}
		if lastRowID == math.MaxInt64 {
			return nil
		}
		afterRowID = lastRowID + 1
	}
}

func sealStorageValue(ctx context.Context, storage *kritui_db.StorageCipher, migration storageMigration, ownerID int64, rowID string, value []byte) (any, error) {
	ownerContext := kritui_db.WithStorageCipher(kritui_db.WithUser(ctx, ownerID), storage)
	switch migration.kind {
	case storageFieldBytes:
		return kritui_db.EncryptStorageBytes(ownerContext, migration.scope, rowID, value)
	case storageFieldProviderOutput:
		envelope, err := kritui_db.EncryptStorageText(ownerContext, migration.scope, rowID, string(value))
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(struct {
			Encrypted string `json:"encrypted"`
		}{Encrypted: envelope})
		if err != nil {
			return nil, err
		}
		return string(encoded), nil
	default:
		return kritui_db.EncryptStorageText(ownerContext, migration.scope, rowID, string(value))
	}
}

// scrubStorageEncryption rebuilds the database file with secure_delete enabled,
// removing logically freed plaintext pages, and clears cleanup_pending only
// after VACUUM succeeds. A failure keeps the pending flag so the next startup
// retries before serving.
func scrubStorageEncryption(ctx context.Context, database *sql.DB) error {
	if _, err := database.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("scrub storage encryption plaintext: %w", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE storage_encryption
		SET cleanup_pending = 0
		WHERE id = 1
	`); err != nil {
		return fmt.Errorf("finish storage encryption cleanup: %w", err)
	}
	return nil
}
