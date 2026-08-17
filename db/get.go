package kritui_db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"strings"

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
	id                      int64
	position                int
	message                 llm.Message
	model                   sql.NullString
	totalTokens             sql.NullInt64
	cost                    sql.NullFloat64
	toolCallID              sql.NullString
	toolCallPositions       []int
	promptAppendPositions   []int
	providerOutputPositions []int
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
		SELECT chats.id, chats.title, chats.created_at, chats.updated_at
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
	chats, err := scanChats(rows)
	if err != nil {
		return nil, err
	}
	if err := loadChatTools(ctx, db, chats); err != nil {
		return nil, err
	}
	return chats, nil
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
			SELECT chats.id, chats.title, chats.created_at, chats.updated_at
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
			SELECT chats.id, chats.title, chats.created_at, chats.updated_at
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
	chats, err := scanChats(rows)
	if err != nil {
		return nil, err
	}
	if err := loadChatTools(ctx, db, chats); err != nil {
		return nil, err
	}
	return chats, nil
}

func scanChats(rows *sql.Rows) ([]Chat, error) {
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		chat := Chat{Tools: []string{}}
		if err := rows.Scan(&chat.ID, &chat.Title, &chat.CreatedAt, &chat.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		chats = append(chats, chat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chats: %w", err)
	}
	return chats, nil
}

func loadChatTools(ctx context.Context, db *sql.DB, chats []Chat) error {
	if len(chats) == 0 {
		return nil
	}
	indexes := make(map[int64]int, len(chats))
	arguments := make([]any, len(chats))
	for index := range chats {
		indexes[chats[index].ID] = index
		arguments[index] = chats[index].ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chats)), ",")
	rows, err := db.QueryContext(ctx, `
		SELECT chat_id, name
		FROM chat_tools
		WHERE chat_id IN (`+placeholders+`)
		ORDER BY chat_id, position
	`, arguments...)
	if err != nil {
		return fmt.Errorf("get tools for chats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var chatID int64
		var name string
		if err := rows.Scan(&chatID, &name); err != nil {
			return fmt.Errorf("scan tool for chat: %w", err)
		}
		if index, ok := indexes[chatID]; ok {
			chats[index].Tools = append(chats[index].Tools, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate tools for chats: %w", err)
	}
	return nil
}

// GetChatTools returns enabled tool names for a chat.
// Missing chats return an empty list.
func GetChatTools(ctx context.Context, db *sql.DB, chatID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM chat_tools WHERE chat_id = ? ORDER BY position`, chatID)
	if err != nil {
		return nil, fmt.Errorf("get chat tools: %w", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan chat tool: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat tools: %w", err)
	}
	return names, nil
}

// GetChatPromptAppendIDs returns selected prompt append IDs for a chat.
// Missing chats return an empty list.
func GetChatPromptAppendIDs(ctx context.Context, db *sql.DB, chatID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT prompt_append_id
		FROM chat_prompt_appends
		WHERE chat_id = ?
		ORDER BY position
	`, chatID)
	if err != nil {
		return nil, fmt.Errorf("get chat prompt append IDs: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan chat prompt append ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat prompt append IDs: %w", err)
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
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MessageSnapshot{}, fmt.Errorf("begin message snapshot: %w", err)
	}
	defer tx.Rollback()
	snapshot, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return MessageSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageSnapshot{}, fmt.Errorf("commit message snapshot: %w", err)
	}
	return snapshot, nil
}

func getMessageSnapshot(ctx context.Context, db messageQueryer, chatID int64) (MessageSnapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, position, role, content, model, total_tokens, cost, tool_call_id
		FROM messages
		WHERE chat_id = ?
		ORDER BY position, id
	`, chatID)
	if err != nil {
		return MessageSnapshot{}, fmt.Errorf("get messages: %w", err)
	}

	storedMessages := []storedMessage{}
	indexes := make(map[int64]int)
	for rows.Next() {
		stored, err := scanStoredMessage(rows)
		if err != nil {
			rows.Close()
			return MessageSnapshot{}, err
		}
		indexes[stored.id] = len(storedMessages)
		storedMessages = append(storedMessages, stored)
	}
	if err := rows.Close(); err != nil {
		return MessageSnapshot{}, fmt.Errorf("close messages: %w", err)
	}
	if err := rows.Err(); err != nil {
		return MessageSnapshot{}, fmt.Errorf("iterate messages: %w", err)
	}

	if err := loadMessageToolCalls(ctx, db, chatID, storedMessages, indexes); err != nil {
		return MessageSnapshot{}, err
	}
	if err := loadMessagePromptAppends(ctx, db, chatID, storedMessages, indexes); err != nil {
		return MessageSnapshot{}, err
	}
	if err := loadMessageProviderOutputs(ctx, db, chatID, storedMessages, indexes); err != nil {
		return MessageSnapshot{}, err
	}

	snapshot := MessageSnapshot{
		chatID:  chatID,
		version: messageVersion{lastPosition: -1},
	}
	digest := sha256.New()
	for _, stored := range storedMessages {
		snapshot.Messages = append(snapshot.Messages, stored.message)
		snapshot.version.count++
		snapshot.version.lastID = stored.id
		snapshot.version.lastPosition = stored.position
		writeStoredMessage(digest, stored)
	}
	copy(snapshot.version.digest[:], digest.Sum(nil))
	return snapshot, nil
}

func scanStoredMessage(row rowScanner) (storedMessage, error) {
	var stored storedMessage
	if err := row.Scan(
		&stored.id,
		&stored.position,
		&stored.message.Role,
		&stored.message.Content,
		&stored.model,
		&stored.totalTokens,
		&stored.cost,
		&stored.toolCallID,
	); err != nil {
		return storedMessage{}, fmt.Errorf("scan message: %w", err)
	}
	stored.message.ID = stored.id
	if stored.model.Valid {
		stored.message.Model = stored.model.String
	}
	if stored.totalTokens.Valid {
		tokens := int(stored.totalTokens.Int64)
		stored.message.TotalTokens = &tokens
	}
	if stored.cost.Valid {
		cost := stored.cost.Float64
		stored.message.Cost = &cost
	}
	if stored.toolCallID.Valid {
		stored.message.ToolCallID = stored.toolCallID.String
	}
	return stored, nil
}

func loadMessageToolCalls(ctx context.Context, db messageQueryer, chatID int64, messages []storedMessage, indexes map[int64]int) error {
	rows, err := db.QueryContext(ctx, `
		SELECT calls.message_id, calls.position, calls.call_id, calls.call_type, calls.function_name, calls.arguments
		FROM message_tool_calls AS calls
		JOIN messages ON messages.id = calls.message_id
		WHERE messages.chat_id = ?
		ORDER BY messages.position, messages.id, calls.position
	`, chatID)
	if err != nil {
		return fmt.Errorf("get message tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var position int
		var call llm.ToolCall
		if err := rows.Scan(&messageID, &position, &call.ID, &call.Type, &call.Function.Name, &call.Function.Arguments); err != nil {
			return fmt.Errorf("scan message tool call: %w", err)
		}
		index, ok := indexes[messageID]
		if !ok {
			return fmt.Errorf("message tool call references unloaded message %d", messageID)
		}
		messages[index].message.ToolCalls = append(messages[index].message.ToolCalls, call)
		messages[index].toolCallPositions = append(messages[index].toolCallPositions, position)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message tool calls: %w", err)
	}
	return nil
}

func loadMessagePromptAppends(ctx context.Context, db messageQueryer, chatID int64, messages []storedMessage, indexes map[int64]int) error {
	rows, err := db.QueryContext(ctx, `
		SELECT appends.message_id, appends.position, appends.text
		FROM message_prompt_appends AS appends
		JOIN messages ON messages.id = appends.message_id
		WHERE messages.chat_id = ?
		ORDER BY messages.position, messages.id, appends.position
	`, chatID)
	if err != nil {
		return fmt.Errorf("get message prompt appends: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var position int
		var text string
		if err := rows.Scan(&messageID, &position, &text); err != nil {
			return fmt.Errorf("scan message prompt append: %w", err)
		}
		index, ok := indexes[messageID]
		if !ok {
			return fmt.Errorf("message prompt append references unloaded message %d", messageID)
		}
		messages[index].message.PromptAppendTexts = append(messages[index].message.PromptAppendTexts, text)
		messages[index].promptAppendPositions = append(messages[index].promptAppendPositions, position)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message prompt appends: %w", err)
	}
	return nil
}

func loadMessageProviderOutputs(ctx context.Context, db messageQueryer, chatID int64, messages []storedMessage, indexes map[int64]int) error {
	rows, err := db.QueryContext(ctx, `
		SELECT outputs.message_id, outputs.position, outputs.payload
		FROM message_provider_outputs AS outputs
		JOIN messages ON messages.id = outputs.message_id
		WHERE messages.chat_id = ?
		ORDER BY messages.position, messages.id, outputs.position
	`, chatID)
	if err != nil {
		return fmt.Errorf("get message provider outputs: %w", err)
	}
	defer rows.Close()
	outputs := make(map[int64][]json.RawMessage)
	for rows.Next() {
		var messageID int64
		var position int
		var payload string
		if err := rows.Scan(&messageID, &position, &payload); err != nil {
			return fmt.Errorf("scan message provider output: %w", err)
		}
		if _, ok := indexes[messageID]; !ok {
			return fmt.Errorf("message provider output references unloaded message %d", messageID)
		}
		outputs[messageID] = append(outputs[messageID], json.RawMessage(payload))
		messages[indexes[messageID]].providerOutputPositions = append(messages[indexes[messageID]].providerOutputPositions, position)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message provider outputs: %w", err)
	}
	for messageID, values := range outputs {
		metadata, err := llm.NewResponsesProviderMetadata(values)
		if err != nil {
			return fmt.Errorf("restore provider metadata for message %d: %w", messageID, err)
		}
		messages[indexes[messageID]].message.ProviderMetadata = metadata
	}
	return nil
}

func writeStoredMessage(digest hash.Hash, stored storedMessage) {
	writeInt64(digest, stored.id)
	writeInt64(digest, int64(stored.position))
	writeString(digest, stored.message.Role)
	writeString(digest, stored.message.Content)
	writeNullString(digest, stored.model)
	writeNullInt64(digest, stored.totalTokens)
	writeNullFloat64(digest, stored.cost)
	writeNullString(digest, stored.toolCallID)
	writeInt64(digest, int64(len(stored.message.ToolCalls)))
	for index, call := range stored.message.ToolCalls {
		writeInt64(digest, int64(stored.toolCallPositions[index]))
		writeString(digest, call.ID)
		writeString(digest, call.Type)
		writeString(digest, call.Function.Name)
		writeString(digest, call.Function.Arguments)
	}
	writeInt64(digest, int64(len(stored.message.PromptAppendTexts)))
	for index, text := range stored.message.PromptAppendTexts {
		writeInt64(digest, int64(stored.promptAppendPositions[index]))
		writeString(digest, text)
	}
	outputs := stored.message.ProviderMetadata.ResponsesOutput()
	writeInt64(digest, int64(len(outputs)))
	for index, output := range outputs {
		writeInt64(digest, int64(stored.providerOutputPositions[index]))
		writeString(digest, string(output))
	}
}

func writeNullString(digest hash.Hash, value sql.NullString) {
	writeBool(digest, value.Valid)
	if value.Valid {
		writeString(digest, value.String)
	}
}

func writeNullInt64(digest hash.Hash, value sql.NullInt64) {
	writeBool(digest, value.Valid)
	if value.Valid {
		writeInt64(digest, value.Int64)
	}
}

func writeNullFloat64(digest hash.Hash, value sql.NullFloat64) {
	writeBool(digest, value.Valid)
	if value.Valid {
		writeUint64(digest, math.Float64bits(value.Float64))
	}
}

func writeString(digest hash.Hash, value string) {
	writeInt64(digest, int64(len(value)))
	_, _ = digest.Write([]byte(value))
}

func writeBool(digest hash.Hash, value bool) {
	if value {
		_, _ = digest.Write([]byte{1})
		return
	}
	_, _ = digest.Write([]byte{0})
}

func writeInt64(digest hash.Hash, value int64) {
	writeUint64(digest, uint64(value))
}

func writeUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}
