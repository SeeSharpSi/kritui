package kritui_db

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed share_schema.sql
var shareSchema string

//go:embed encrypted_share_schema.sql
var encryptedShareSchema string

const (
	// SharedResponseFormatVersion is the only supported encrypted share payload
	// format written by this package.
	SharedResponseFormatVersion = 1
	// MaxSharedResponseBytes bounds the encrypted payload stored for one share.
	MaxSharedResponseBytes = 16 << 20
)

var (
	// ErrShareConflict means a token already belongs to a different share and
	// must not overwrite the existing ciphertext.
	ErrShareConflict = errors.New("shared response conflict")
)

// ShareSource is the minimal source content needed to render one immutable
// shared response.
type ShareSource struct {
	Title       string
	UserContent string
	Content     string
}

// SharedResponse is an immutable encrypted share payload. ChatID and MessageID
// are metadata only; the ciphertext remains readable if the source is edited or
// deleted. Encryption keys are never stored here or in the database.
type SharedResponse struct {
	TokenHash     string
	ChatID        int64
	MessageID     int64
	FormatVersion int
	Nonce         []byte
	Ciphertext    []byte
}

// MigrateSharedResponses creates the legacy version 29 plaintext shared-response
// table. It remains available so historical migration 29 stays reproducible;
// fresh databases use MigrateEncryptedSharedResponses instead.
func MigrateSharedResponses(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, shareSchema); err != nil {
		return fmt.Errorf("migrate shared responses schema: %w", err)
	}
	return nil
}

// MigrateEncryptedSharedResponses performs schema version 30. A legacy version
// 29 plaintext share table is retired by rebuilding it as the encrypted table,
// which deletes its plaintext rows. Chats, messages, users, settings, and every
// unrelated table are left untouched. An existing encrypted table is a no-op so
// repeated migrations preserve published ciphertext.
func MigrateEncryptedSharedResponses(ctx context.Context, tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'shared_responses'
	`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect shared responses table: %w", err)
	}
	if exists == 0 {
		if _, err := tx.ExecContext(ctx, encryptedShareSchema); err != nil {
			return fmt.Errorf("migrate encrypted shared responses schema: %w", err)
		}
		return nil
	}

	var legacyColumns int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_table_info('shared_responses') WHERE name = 'html'
	`).Scan(&legacyColumns); err != nil {
		return fmt.Errorf("inspect shared responses columns: %w", err)
	}
	if legacyColumns == 0 {
		var encryptedColumns int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pragma_table_info('shared_responses')
			WHERE name IN ('format_version', 'nonce', 'ciphertext')
		`).Scan(&encryptedColumns); err != nil {
			return fmt.Errorf("inspect encrypted shared responses columns: %w", err)
		}
		if encryptedColumns != 3 {
			return fmt.Errorf("migrate encrypted shared responses schema: shared_responses has an unexpected shape")
		}
		return nil
	}

	var secureDelete int
	if err := tx.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
		return fmt.Errorf("read secure_delete mode: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA secure_delete = 1`); err != nil {
		return fmt.Errorf("enable secure deletion: %w", err)
	}
	restore := func() error {
		if _, err := tx.ExecContext(ctx, `PRAGMA secure_delete = `+secureDeleteModeName(secureDelete)); err != nil {
			return fmt.Errorf("restore secure_delete mode: %w", err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE shared_responses`); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("drop legacy shared responses: %w; %v", err, restoreErr)
		}
		return fmt.Errorf("drop legacy shared responses: %w", err)
	}
	if _, err := tx.ExecContext(ctx, encryptedShareSchema); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("create encrypted shared responses: %w; %v", err, restoreErr)
		}
		return fmt.Errorf("create encrypted shared responses: %w", err)
	}
	if err := restore(); err != nil {
		return err
	}
	return nil
}

// secureDeleteModeName maps the value returned by PRAGMA secure_delete back to
// the pragma spelling that restores the exact mode. SQLite accepts 0, 1, and 2,
// where 2 is the FAST mode rather than a second enabled state.
func secureDeleteModeName(mode int) string {
	switch mode {
	case 0:
		return "OFF"
	case 1:
		return "ON"
	case 2:
		return "FAST"
	default:
		return fmt.Sprintf("%d", mode)
	}
}

// GetShareSource loads the title, nearest preceding active user-message text,
// and content of one active, final assistant message owned by the explicit user
// in ctx. Foreign, hidden, non-assistant, tool-call, and missing messages all
// return ErrMessageNotFound. A target without a preceding user message keeps an
// empty UserContent.
func GetShareSource(ctx context.Context, database *sql.DB, chatID, messageID int64) (ShareSource, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return ShareSource{}, fmt.Errorf("get share source: %w", err)
	}
	var (
		source       ShareSource
		priorID      sql.NullInt64
		priorContent sql.NullString
	)
	err = database.QueryRowContext(ctx, `
		SELECT chats.title, prior.id, prior.content, messages.content
		FROM messages
		JOIN chats ON chats.id = messages.chat_id
		LEFT JOIN messages AS prior ON prior.id = (
			SELECT candidate.id
			FROM messages AS candidate
			WHERE candidate.chat_id = messages.chat_id
				AND candidate.role = 'user'
				AND candidate.undo_sequence IS NULL
				AND candidate.position < messages.position
			ORDER BY candidate.position DESC
			LIMIT 1
		)
		WHERE chats.user_id = ?
			AND messages.chat_id = ?
			AND messages.id = ?
			AND messages.undo_sequence IS NULL
			AND messages.role = 'assistant'
			AND NOT EXISTS (
				SELECT 1 FROM message_tool_calls WHERE message_tool_calls.message_id = messages.id
			)
	`, userID, chatID, messageID).Scan(&source.Title, &priorID, &priorContent, &source.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareSource{}, ErrMessageNotFound
	}
	if err != nil {
		return ShareSource{}, fmt.Errorf("get share source: %w", err)
	}
	source.Title, err = openChatTitle(ctx, chatID, source.Title)
	if err != nil {
		return ShareSource{}, fmt.Errorf("get share source title: %w", err)
	}
	source.Content, err = DecryptStorageText(ctx, storageScopeMessageContent, messageStorageRowID(messageID), source.Content)
	if err != nil {
		return ShareSource{}, fmt.Errorf("get share source content: %w", err)
	}
	if priorID.Valid {
		if !priorContent.Valid {
			return ShareSource{}, fmt.Errorf("get share source: preceding user message %d has no content", priorID.Int64)
		}
		source.UserContent, err = DecryptStorageText(ctx, storageScopeMessageContent, messageStorageRowID(priorID.Int64), priorContent.String)
		if err != nil {
			return ShareSource{}, fmt.Errorf("get share source user content: %w", err)
		}
	}
	return source, nil
}

// CreateSharedResponse stores one immutable encrypted snapshot. Ownership and
// the active final assistant constraints are revalidated in the same statement,
// so a source hidden or removed after the caller read it cannot be published.
// Repeating an identical publication is idempotent even after the source is
// removed; any differing payload or owner for the same token returns
// ErrShareConflict and never overwrites stored ciphertext.
func CreateSharedResponse(ctx context.Context, database *sql.DB, share SharedResponse) error {
	userID, err := requireUserID(ctx)
	if err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	if strings.TrimSpace(share.TokenHash) == "" {
		return errors.New("create shared response: token hash is required")
	}
	if share.FormatVersion != SharedResponseFormatVersion {
		return fmt.Errorf("create shared response: format version must be %d", SharedResponseFormatVersion)
	}
	if len(share.Nonce) != 12 {
		return errors.New("create shared response: nonce must be 12 bytes")
	}
	if len(share.Ciphertext) < 16 || len(share.Ciphertext) > MaxSharedResponseBytes {
		return fmt.Errorf("create shared response: ciphertext must contain between 16 and %d bytes", MaxSharedResponseBytes)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	defer tx.Rollback()

	existing, found, err := getSharedResponseTx(ctx, tx, share.TokenHash)
	if err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	if found {
		return finishSharedResponseCreate(tx, existing, userID, share)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO shared_responses (
			token_hash, user_id, chat_id, message_id,
			format_version, nonce, ciphertext
		)
		SELECT ?, ?, chats.id, messages.id, ?, ?, ?
		FROM messages
		JOIN chats ON chats.id = messages.chat_id
		WHERE chats.user_id = ?
			AND messages.chat_id = ?
			AND messages.id = ?
			AND messages.undo_sequence IS NULL
			AND messages.role = 'assistant'
			AND NOT EXISTS (
				SELECT 1 FROM message_tool_calls WHERE message_tool_calls.message_id = messages.id
			)
		ON CONFLICT(token_hash) DO NOTHING
	`, share.TokenHash, userID, share.FormatVersion, share.Nonce, share.Ciphertext,
		userID, share.ChatID, share.MessageID)
	if err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	if inserted == 0 {
		existing, found, err = getSharedResponseTx(ctx, tx, share.TokenHash)
		if err != nil {
			return fmt.Errorf("create shared response: %w", err)
		}
		if found {
			return finishSharedResponseCreate(tx, existing, userID, share)
		}
		return ErrMessageNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	return nil
}

func finishSharedResponseCreate(tx *sql.Tx, existing storedSharedResponse, userID int64, share SharedResponse) error {
	if existing.ownerID != userID ||
		existing.response.ChatID != share.ChatID ||
		existing.response.MessageID != share.MessageID ||
		existing.response.FormatVersion != share.FormatVersion ||
		!bytes.Equal(existing.response.Nonce, share.Nonce) ||
		!bytes.Equal(existing.response.Ciphertext, share.Ciphertext) {
		return ErrShareConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create shared response: %w", err)
	}
	return nil
}

type storedSharedResponse struct {
	ownerID  int64
	response SharedResponse
}

func getSharedResponseTx(ctx context.Context, tx *sql.Tx, tokenHash string) (storedSharedResponse, bool, error) {
	var stored storedSharedResponse
	stored.response.TokenHash = tokenHash
	err := tx.QueryRowContext(ctx, `
		SELECT user_id, chat_id, message_id, format_version, nonce, ciphertext
		FROM shared_responses
		WHERE token_hash = ?
	`, tokenHash).Scan(
		&stored.ownerID,
		&stored.response.ChatID,
		&stored.response.MessageID,
		&stored.response.FormatVersion,
		&stored.response.Nonce,
		&stored.response.Ciphertext,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedSharedResponse{}, false, nil
	}
	if err != nil {
		return storedSharedResponse{}, false, err
	}
	return stored, true, nil
}

// GetSharedResponse returns one stored encrypted snapshot by token hash. It is
// intentionally public and does not consult a user context. The response
// contains no key material.
func GetSharedResponse(ctx context.Context, database *sql.DB, tokenHash string) (SharedResponse, error) {
	var response SharedResponse
	err := database.QueryRowContext(ctx, `
		SELECT token_hash, chat_id, message_id, format_version, nonce, ciphertext
		FROM shared_responses
		WHERE token_hash = ?
	`, tokenHash).Scan(
		&response.TokenHash,
		&response.ChatID,
		&response.MessageID,
		&response.FormatVersion,
		&response.Nonce,
		&response.Ciphertext,
	)
	if err != nil {
		return SharedResponse{}, err
	}
	return response, nil
}
