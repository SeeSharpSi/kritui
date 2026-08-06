package kritui_db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"

	"seesharpsi/kritui/llm"
)

// Chat contains metadata for a stored chat.
type Chat struct {
	ID        int64
	Title     string
	Tools     []string
	CreatedAt string
	UpdatedAt string
}

// MessageSnapshot contains ordered messages and the storage version used for
// optimistic completion persistence.
type MessageSnapshot struct {
	Messages []llm.Message
	chatID   int64
	version  messageVersion
}

type messageVersion struct {
	count        int
	lastID       int64
	lastPosition int
	digest       [sha256.Size]byte
}

type storedMessage struct {
	id          int64
	position    int
	message     llm.Message
	fingerprint string
}

type rowScanner interface {
	Scan(...any) error
}

type messageQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// GetChats returns chats from most recently updated to least recently updated.
func GetChats(ctx context.Context, db *sql.DB) ([]Chat, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT chats.id, chats.title, chats.tools, chats.created_at, chats.updated_at
		FROM chats
		WHERE EXISTS (
			SELECT 1
			FROM messages
			WHERE messages.chat_id = chats.id
		)
		ORDER BY chats.updated_at DESC, chats.id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("get chats: %w", err)
	}
	return scanChats(rows)
}

// GetChatsPage returns chats after the supplied cursor, ordered from most to
// least recently updated. An empty cursor returns the first page.
func GetChatsPage(ctx context.Context, db *sql.DB, beforeUpdatedAt string, beforeID int64, limit int) ([]Chat, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("get chats page: limit must be positive")
	}

	var (
		rows *sql.Rows
		err  error
	)
	if beforeUpdatedAt == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT chats.id, chats.title, chats.tools, chats.created_at, chats.updated_at
			FROM chats
			WHERE EXISTS (
				SELECT 1
				FROM messages
				WHERE messages.chat_id = chats.id
			)
			ORDER BY chats.updated_at DESC, chats.id DESC
			LIMIT ?
		`, limit)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT chats.id, chats.title, chats.tools, chats.created_at, chats.updated_at
			FROM chats
			WHERE EXISTS (
				SELECT 1
				FROM messages
				WHERE messages.chat_id = chats.id
			)
				AND (chats.updated_at < ? OR (chats.updated_at = ? AND chats.id < ?))
			ORDER BY chats.updated_at DESC, chats.id DESC
			LIMIT ?
		`, beforeUpdatedAt, beforeUpdatedAt, beforeID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("get chats page: %w", err)
	}
	return scanChats(rows)
}

func scanChats(rows *sql.Rows) ([]Chat, error) {
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var tools string
		if err := rows.Scan(&chat.ID, &chat.Title, &tools, &chat.CreatedAt, &chat.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		names, err := decodeToolNames(tools)
		if err != nil {
			return nil, fmt.Errorf("decode chat tools: %w", err)
		}
		chat.Tools = names
		chats = append(chats, chat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chats: %w", err)
	}

	return chats, nil
}

// GetChatTools returns the enabled tool names for a chat.
// Missing chats return an empty list.
func GetChatTools(ctx context.Context, db *sql.DB, chatID int64) ([]string, error) {
	var tools string
	err := db.QueryRowContext(ctx, `SELECT tools FROM chats WHERE id = ?`, chatID).Scan(&tools)
	if err == sql.ErrNoRows {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get chat tools: %w", err)
	}

	names, err := decodeToolNames(tools)
	if err != nil {
		return nil, fmt.Errorf("decode chat tools: %w", err)
	}
	return names, nil
}

// GetChatPromptAppendIDs returns the selected prompt append IDs for a chat.
// Missing chats return an empty list.
func GetChatPromptAppendIDs(ctx context.Context, db *sql.DB, chatID int64) ([]string, error) {
	var encodedAppendIDs string
	err := db.QueryRowContext(ctx, `SELECT appends FROM chats WHERE id = ?`, chatID).Scan(&encodedAppendIDs)
	if err == sql.ErrNoRows {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get chat prompt append IDs: %w", err)
	}

	ids, err := decodePromptAppendIDs(encodedAppendIDs)
	if err != nil {
		return nil, fmt.Errorf("decode chat prompt append IDs: %w", err)
	}
	return ids, nil
}

// GetMessages returns a chat's messages in conversation order.
func GetMessages(ctx context.Context, db *sql.DB, chatID int64) ([]llm.Message, error) {
	snapshot, err := GetMessageSnapshot(ctx, db, chatID)
	if err != nil {
		return nil, err
	}
	return snapshot.Messages, nil
}

// GetMessageSnapshot returns messages and an optimistic storage version.
func GetMessageSnapshot(ctx context.Context, db *sql.DB, chatID int64) (MessageSnapshot, error) {
	return getMessageSnapshot(ctx, db, chatID)
}

func getMessageSnapshot(ctx context.Context, db messageQueryer, chatID int64) (MessageSnapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			id,
			position,
			role,
			content,
			model,
			total_tokens,
			cost,
			tool_calls,
			tool_call_id,
			provider_metadata,
			prompt_appends,
			json_array(id, position, role, content, model, total_tokens, cost, tool_calls, tool_call_id, provider_metadata, prompt_appends)
		FROM messages
		WHERE chat_id = ?
		ORDER BY position, id
	`, chatID)
	if err != nil {
		return MessageSnapshot{}, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()

	snapshot := MessageSnapshot{
		chatID:  chatID,
		version: messageVersion{lastPosition: -1},
	}
	digest := sha256.New()
	for rows.Next() {
		stored, err := scanStoredMessage(rows)
		if err != nil {
			return MessageSnapshot{}, err
		}
		snapshot.Messages = append(snapshot.Messages, stored.message)
		snapshot.version.count++
		snapshot.version.lastID = stored.id
		snapshot.version.lastPosition = stored.position
		_, _ = digest.Write([]byte(stored.fingerprint))
		_, _ = digest.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return MessageSnapshot{}, fmt.Errorf("iterate messages: %w", err)
	}

	copy(snapshot.version.digest[:], digest.Sum(nil))
	return snapshot, nil
}

func scanStoredMessage(row rowScanner) (storedMessage, error) {
	var stored storedMessage
	var model sql.NullString
	var totalTokens sql.NullInt64
	var cost sql.NullFloat64
	var toolCalls sql.NullString
	var toolCallID sql.NullString
	var providerMetadata sql.NullString
	var promptAppendTexts sql.NullString
	if err := row.Scan(
		&stored.id,
		&stored.position,
		&stored.message.Role,
		&stored.message.Content,
		&model,
		&totalTokens,
		&cost,
		&toolCalls,
		&toolCallID,
		&providerMetadata,
		&promptAppendTexts,
		&stored.fingerprint,
	); err != nil {
		return storedMessage{}, fmt.Errorf("scan message: %w", err)
	}

	if model.Valid {
		stored.message.Model = model.String
	}
	if totalTokens.Valid {
		tokens := int(totalTokens.Int64)
		stored.message.TotalTokens = &tokens
	}
	if cost.Valid {
		value := cost.Float64
		stored.message.Cost = &value
	}
	if toolCalls.Valid {
		if err := json.Unmarshal([]byte(toolCalls.String), &stored.message.ToolCalls); err != nil {
			return storedMessage{}, fmt.Errorf("decode message tool calls: %w", err)
		}
	}
	if toolCallID.Valid {
		stored.message.ToolCallID = toolCallID.String
	}
	if providerMetadata.Valid {
		if stored.message.Role != "assistant" {
			return storedMessage{}, fmt.Errorf("decode provider metadata: only assistant messages may contain provider metadata")
		}
		if err := json.Unmarshal([]byte(providerMetadata.String), &stored.message.ProviderMetadata); err != nil {
			return storedMessage{}, fmt.Errorf("decode provider metadata: %w", err)
		}
	}
	if promptAppendTexts.Valid {
		if stored.message.Role != "user" {
			return storedMessage{}, fmt.Errorf("decode prompt append texts: only user messages may contain prompt append texts")
		}
		if err := json.Unmarshal([]byte(promptAppendTexts.String), &stored.message.PromptAppendTexts); err != nil {
			return storedMessage{}, fmt.Errorf("decode prompt append texts: %w", err)
		}
	}

	return stored, nil
}
