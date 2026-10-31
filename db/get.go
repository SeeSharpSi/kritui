package kritui_db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	Pinned    bool
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
	attachmentPositions     []int
}

type rowScanner interface {
	Scan(...any) error
}

type messageQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const (
	storageScopeChatTitle          = "chats.title"
	storageScopeMessageContent     = "messages.content"
	storageScopeToolCallArguments  = "message_tool_calls.arguments"
	storageScopePromptAppendText   = "message_prompt_appends.text"
	storageScopeAttachmentFilename = "message_user_images.filename"
	storageScopeAttachmentData     = "message_user_images.data"
	storageScopeProviderOutput     = "message_provider_outputs.payload"
)

func chatStorageRowID(chatID int64) string {
	return fmt.Sprintf("%d", chatID)
}

func messageStorageRowID(messageID int64) string {
	return fmt.Sprintf("%d", messageID)
}

func messageChildStorageRowID(messageID int64, position int) string {
	return fmt.Sprintf("%d:%d", messageID, position)
}

// sealChatTitle encrypts a non-empty title for its chat row. The empty string
// is the allocated-chat sentinel and stays plaintext so title discovery still
// works before a title exists.
func sealChatTitle(ctx context.Context, chatID int64, title string) (string, error) {
	if title == "" {
		return "", nil
	}
	return EncryptStorageText(ctx, storageScopeChatTitle, chatStorageRowID(chatID), title)
}

func openChatTitle(ctx context.Context, chatID int64, title string) (string, error) {
	if title == "" {
		return "", nil
	}
	return DecryptStorageText(ctx, storageScopeChatTitle, chatStorageRowID(chatID), title)
}

// sealProviderOutput keeps legacy provider payloads as their original JSON
// object. With storage encryption enabled, the payload becomes a strict JSON
// object carrying the text envelope so the schema object check still applies.
func sealProviderOutput(ctx context.Context, messageID int64, position int, payload []byte) (string, error) {
	rowID := messageChildStorageRowID(messageID, position)
	envelope, err := EncryptStorageText(ctx, storageScopeProviderOutput, rowID, string(payload))
	if err != nil {
		return "", err
	}
	if envelope == string(payload) {
		return envelope, nil
	}
	encoded, err := json.Marshal(struct {
		Encrypted string `json:"encrypted"`
	}{Encrypted: envelope})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func openProviderOutput(ctx context.Context, messageID int64, position int, payload string) (string, error) {
	rowID := messageChildStorageRowID(messageID, position)
	plain, err := DecryptStorageText(ctx, storageScopeProviderOutput, rowID, payload)
	if err == nil {
		return plain, nil
	}
	if !errors.Is(err, ErrStorageDecryption) {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return "", ErrStorageDecryption
	}
	raw, ok := fields["encrypted"]
	if !ok || len(fields) != 1 {
		return "", ErrStorageDecryption
	}
	var envelope string
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", ErrStorageDecryption
	}
	return DecryptStorageText(ctx, storageScopeProviderOutput, rowID, envelope)
}

// GetChatsPage returns chats after the supplied cursor, ordered with pinned
// chats first and then from most to least recently updated within each group.
// An empty cursor returns the first page.
func GetChatsPage(ctx context.Context, db *sql.DB, beforeUpdatedAt string, beforeID int64, beforePinned bool, limit int) ([]Chat, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("get chats page: limit must be positive")
	}
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get chats page: %w", err)
	}

	var (
		rows *sql.Rows
	)
	if beforeUpdatedAt == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT chats.id, chats.title, chats.created_at, chats.updated_at, chats.pinned
			FROM chats
			WHERE chats.user_id = ?
				AND EXISTS (
					SELECT 1
					FROM messages
					WHERE messages.chat_id = chats.id
				)
			ORDER BY chats.pinned DESC, chats.updated_at DESC, chats.id DESC
			LIMIT ?
		`, userID, limit)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT chats.id, chats.title, chats.created_at, chats.updated_at, chats.pinned
			FROM chats
			WHERE chats.user_id = ?
				AND EXISTS (
					SELECT 1
					FROM messages
					WHERE messages.chat_id = chats.id
				)
				AND (
					chats.pinned < ?
					OR (
						chats.pinned = ?
						AND (
							chats.updated_at < ?
							OR (chats.updated_at = ? AND chats.id < ?)
						)
					)
				)
			ORDER BY chats.pinned DESC, chats.updated_at DESC, chats.id DESC
			LIMIT ?
		`, userID, beforePinned, beforePinned, beforeUpdatedAt, beforeUpdatedAt, beforeID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("get chats page: %w", err)
	}
	chats, err := scanChats(ctx, rows)
	if err != nil {
		return nil, err
	}
	if err := loadChatTools(ctx, db, chats); err != nil {
		return nil, err
	}
	return chats, nil
}

func scanChats(ctx context.Context, rows *sql.Rows) ([]Chat, error) {
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		chat := Chat{Tools: []string{}}
		var pinned int
		if err := rows.Scan(&chat.ID, &chat.Title, &chat.CreatedAt, &chat.UpdatedAt, &pinned); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		title, err := openChatTitle(ctx, chat.ID, chat.Title)
		if err != nil {
			return nil, fmt.Errorf("decrypt chat title %d: %w", chat.ID, err)
		}
		chat.Title = title
		chat.Pinned = pinned != 0
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
	userID, err := requireUserID(ctx)
	if err != nil {
		return fmt.Errorf("get tools for chats: %w", err)
	}
	indexes := make(map[int64]int, len(chats))
	arguments := make([]any, 0, len(chats)+1)
	arguments = append(arguments, userID)
	for index := range chats {
		indexes[chats[index].ID] = index
		arguments = append(arguments, chats[index].ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chats)), ",")
	rows, err := db.QueryContext(ctx, `
		SELECT tools.chat_id, tools.name
		FROM chat_tools AS tools
		JOIN chats ON chats.id = tools.chat_id AND chats.user_id = ?
		WHERE tools.chat_id IN (`+placeholders+`)
		ORDER BY tools.chat_id, tools.position
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

// GetChatTitle returns the stored title for an owned chat.
func GetChatTitle(ctx context.Context, db *sql.DB, chatID int64) (string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return "", fmt.Errorf("get chat title: %w", err)
	}
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM chats WHERE id = ? AND user_id = ?`, chatID, userID).Scan(&title); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrChatNotFound
		}
		return "", fmt.Errorf("get chat title: %w", err)
	}
	title, err = openChatTitle(ctx, chatID, title)
	if err != nil {
		return "", fmt.Errorf("get chat title: %w", err)
	}
	return title, nil
}

// GetChatTools returns enabled tool names for a chat.
// Missing chats return an empty list.
func GetChatTools(ctx context.Context, db *sql.DB, chatID int64) ([]string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get chat tools: %w", err)
	}
	access, err := chatAccessFor(ctx, db, chatID)
	if err != nil {
		return nil, fmt.Errorf("get chat tools: %w", err)
	}
	if access == chatForeign {
		return nil, ErrChatNotFound
	}
	if access == chatMissing {
		return []string{}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT tools.name
		FROM chat_tools AS tools
		JOIN chats ON chats.id = tools.chat_id AND chats.user_id = ?
		WHERE tools.chat_id = ?
		ORDER BY tools.position
	`, userID, chatID)
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
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get chat prompt append IDs: %w", err)
	}
	access, err := chatAccessFor(ctx, db, chatID)
	if err != nil {
		return nil, fmt.Errorf("get chat prompt append IDs: %w", err)
	}
	if access == chatForeign {
		return nil, ErrChatNotFound
	}
	if access == chatMissing {
		return []string{}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT appends.prompt_append_id
		FROM chat_prompt_appends AS appends
		JOIN chats ON chats.id = appends.chat_id AND chats.user_id = ?
		WHERE appends.chat_id = ?
		ORDER BY appends.position
	`, userID, chatID)
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
	access, err := chatAccessFor(ctx, db, chatID)
	if err != nil {
		return MessageSnapshot{}, fmt.Errorf("get messages: %w", err)
	}
	if access == chatForeign {
		return MessageSnapshot{}, ErrChatNotFound
	}
	if access == chatMissing {
		return MessageSnapshot{chatID: chatID, version: messageVersion{lastPosition: -1}}, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, position, role, content, model, total_tokens, cost, tool_call_id
		FROM messages
		WHERE chat_id = ? AND undo_sequence IS NULL
		ORDER BY position, id
	`, chatID)
	if err != nil {
		return MessageSnapshot{}, fmt.Errorf("get messages: %w", err)
	}

	storedMessages := []storedMessage{}
	indexes := make(map[int64]int)
	for rows.Next() {
		stored, err := scanStoredMessage(ctx, rows)
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
	if err := loadMessageAttachments(ctx, db, chatID, storedMessages, indexes); err != nil {
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

// GetMessageAttachment returns one attachment from an active message in a chat
// owned by the explicit user in ctx. Foreign, hidden, and missing attachments
// all return ErrMessageNotFound so callers cannot distinguish them.
func GetMessageAttachment(ctx context.Context, database *sql.DB, chatID, messageID int64, position int) (llm.UserAttachment, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return llm.UserAttachment{}, fmt.Errorf("get message attachment: %w", err)
	}
	var (
		attachment llm.UserAttachment
		width      int
		height     int
	)
	err = database.QueryRowContext(ctx, `
		SELECT attachments.filename, attachments.media_type, attachments.width, attachments.height, attachments.data
		FROM message_user_images AS attachments
		JOIN messages ON messages.id = attachments.message_id
		JOIN chats ON chats.id = messages.chat_id
		WHERE chats.user_id = ?
			AND messages.chat_id = ?
			AND messages.id = ?
			AND attachments.position = ?
			AND messages.undo_sequence IS NULL
	`, userID, chatID, messageID, position).Scan(
		&attachment.Filename,
		&attachment.MediaType,
		&width,
		&height,
		&attachment.Data,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return llm.UserAttachment{}, ErrMessageNotFound
	}
	if err != nil {
		return llm.UserAttachment{}, fmt.Errorf("get message attachment: %w", err)
	}
	rowID := messageChildStorageRowID(messageID, position)
	attachment.Filename, err = DecryptStorageText(ctx, storageScopeAttachmentFilename, rowID, attachment.Filename)
	if err != nil {
		return llm.UserAttachment{}, fmt.Errorf("get message attachment filename: %w", err)
	}
	attachment.Data, err = DecryptStorageBytes(ctx, storageScopeAttachmentData, rowID, attachment.Data)
	if err != nil {
		return llm.UserAttachment{}, fmt.Errorf("get message attachment data: %w", err)
	}
	attachment.Width, attachment.Height = width, height
	return attachment, nil
}

// loadMessageAttachments restores ordered image and PDF attachments from the
// shared message_user_images table. PDFs carry zero dimensions.
func loadMessageAttachments(ctx context.Context, db messageQueryer, chatID int64, messages []storedMessage, indexes map[int64]int) error {
	rows, err := db.QueryContext(ctx, `
		SELECT attachments.message_id, attachments.position, attachments.filename, attachments.media_type, attachments.width, attachments.height, attachments.data
		FROM message_user_images AS attachments JOIN messages ON messages.id = attachments.message_id
		WHERE messages.chat_id = ? AND messages.undo_sequence IS NULL
		ORDER BY messages.position, messages.id, attachments.position
	`, chatID)
	if err != nil {
		return fmt.Errorf("get message attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var position, width, height int
		var attachment llm.UserAttachment
		if err := rows.Scan(&id, &position, &attachment.Filename, &attachment.MediaType, &width, &height, &attachment.Data); err != nil {
			return fmt.Errorf("scan message attachment: %w", err)
		}
		index, ok := indexes[id]
		if !ok {
			return fmt.Errorf("message attachment references unloaded message %d", id)
		}
		rowID := messageChildStorageRowID(id, position)
		attachment.Filename, err = DecryptStorageText(ctx, storageScopeAttachmentFilename, rowID, attachment.Filename)
		if err != nil {
			return fmt.Errorf("decrypt attachment filename for message %d: %w", id, err)
		}
		attachment.Data, err = DecryptStorageBytes(ctx, storageScopeAttachmentData, rowID, attachment.Data)
		if err != nil {
			return fmt.Errorf("decrypt attachment data for message %d: %w", id, err)
		}
		attachment.Width, attachment.Height = width, height
		attachment.Data = append([]byte(nil), attachment.Data...)
		messages[index].message.Attachments = append(messages[index].message.Attachments, attachment)
		messages[index].attachmentPositions = append(messages[index].attachmentPositions, position)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message attachments: %w", err)
	}
	return nil
}

func scanStoredMessage(ctx context.Context, row rowScanner) (storedMessage, error) {
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
	content, err := DecryptStorageText(ctx, storageScopeMessageContent, messageStorageRowID(stored.id), stored.message.Content)
	if err != nil {
		return storedMessage{}, fmt.Errorf("decrypt message %d content: %w", stored.id, err)
	}
	stored.message.Content = content
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
		SELECT calls.message_id, calls.position, calls.call_id, calls.call_type, calls.function_name, calls.arguments, calls.result_elided
		FROM message_tool_calls AS calls
		JOIN messages ON messages.id = calls.message_id
		WHERE messages.chat_id = ? AND messages.undo_sequence IS NULL
		ORDER BY messages.position, messages.id, calls.position
	`, chatID)
	if err != nil {
		return fmt.Errorf("get message tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var position int
		var resultElided int
		var call llm.ToolCall
		if err := rows.Scan(&messageID, &position, &call.ID, &call.Type, &call.Function.Name, &call.Function.Arguments, &resultElided); err != nil {
			return fmt.Errorf("scan message tool call: %w", err)
		}
		call.ResultElided = resultElided != 0
		index, ok := indexes[messageID]
		if !ok {
			return fmt.Errorf("message tool call references unloaded message %d", messageID)
		}
		arguments, err := DecryptStorageText(ctx, storageScopeToolCallArguments, messageChildStorageRowID(messageID, position), call.Function.Arguments)
		if err != nil {
			return fmt.Errorf("decrypt message tool call %d for message %d: %w", position, messageID, err)
		}
		call.Function.Arguments = arguments
		messages[index].message.ToolCalls = append(messages[index].message.ToolCalls, call)
		messages[index].toolCallPositions = append(messages[index].toolCallPositions, position)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message tool calls: %w", err)
	}
	return nil
}

// GetMessagePromptAppendTexts returns the prompt append snapshot for one
// active user message. Callers can preserve hidden auto-applied appends when
// replacing the message.
func GetMessagePromptAppendTexts(ctx context.Context, db messageQueryer, chatID, messageID int64) ([]string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get message prompt appends: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT appends.position, appends.text
		FROM message_prompt_appends AS appends
		JOIN messages ON messages.id = appends.message_id
		JOIN chats ON chats.id = messages.chat_id
		WHERE chats.user_id = ?
			AND messages.chat_id = ?
			AND messages.id = ?
			AND messages.undo_sequence IS NULL
		ORDER BY appends.position
	`, userID, chatID, messageID)
	if err != nil {
		return nil, fmt.Errorf("get message prompt appends: %w", err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var position int
		var text string
		if err := rows.Scan(&position, &text); err != nil {
			return nil, fmt.Errorf("scan message prompt append: %w", err)
		}
		text, err := DecryptStorageText(ctx, storageScopePromptAppendText, messageChildStorageRowID(messageID, position), text)
		if err != nil {
			return nil, fmt.Errorf("decrypt message prompt append %d for message %d: %w", position, messageID, err)
		}
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message prompt appends: %w", err)
	}
	return texts, nil
}

func loadMessagePromptAppends(ctx context.Context, db messageQueryer, chatID int64, messages []storedMessage, indexes map[int64]int) error {
	rows, err := db.QueryContext(ctx, `
		SELECT appends.message_id, appends.position, appends.text
		FROM message_prompt_appends AS appends
		JOIN messages ON messages.id = appends.message_id
		WHERE messages.chat_id = ? AND messages.undo_sequence IS NULL
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
		text, err := DecryptStorageText(ctx, storageScopePromptAppendText, messageChildStorageRowID(messageID, position), text)
		if err != nil {
			return fmt.Errorf("decrypt message prompt append %d for message %d: %w", position, messageID, err)
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
		WHERE messages.chat_id = ? AND messages.undo_sequence IS NULL
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
		payload, err := openProviderOutput(ctx, messageID, position, payload)
		if err != nil {
			return fmt.Errorf("decrypt provider output %d for message %d: %w", position, messageID, err)
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
	writeInt64(digest, int64(len(stored.message.Attachments)))
	for index, attachment := range stored.message.Attachments {
		writeInt64(digest, int64(stored.attachmentPositions[index]))
		writeString(digest, attachment.Filename)
		writeString(digest, attachment.MediaType)
		writeInt64(digest, int64(attachment.Width))
		writeInt64(digest, int64(attachment.Height))
		writeInt64(digest, int64(len(attachment.Data)))
		_, _ = digest.Write(attachment.Data)
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
