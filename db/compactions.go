package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	chatCompactionSummaryScope = "chat_compactions.summary"
	maxCompactionSummaryBytes  = 256 << 10
)

// ChatCompaction is one durable conversation summary. Messages before
// KeepMessageID are represented by Summary in outbound requests; stored
// history is never rewritten.
type ChatCompaction struct {
	ID            int64
	ChatID        int64
	ProviderID    int64
	Model         string
	KeepMessageID int64
	LastMessageID int64
	Summary       string
}

// CreateChatCompaction stores a summary checkpoint after verifying that the
// chat, kept message, and provider all belong to the current account.
func CreateChatCompaction(ctx context.Context, database *sql.DB, chatID, providerID int64, model, summary string, keepMessageID int64) (ChatCompaction, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ChatCompaction{}, fmt.Errorf("create chat compaction: summary is required")
	}
	if len([]byte(summary)) > maxCompactionSummaryBytes {
		return ChatCompaction{}, fmt.Errorf("create chat compaction: summary must not exceed %d bytes", maxCompactionSummaryBytes)
	}
	model = strings.TrimSpace(model)
	if len([]byte(model)) > 512 {
		return ChatCompaction{}, fmt.Errorf("create chat compaction: model must not exceed 512 bytes")
	}
	userID, err := requireUserID(ctx)
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("create chat compaction: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("begin chat compaction: %w", err)
	}
	defer tx.Rollback()

	var owner int64
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM chats WHERE id = ?`, chatID).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatCompaction{}, ErrChatNotFound
		}
		return ChatCompaction{}, fmt.Errorf("check compaction chat: %w", err)
	}
	if owner != userID {
		return ChatCompaction{}, ErrChatNotFound
	}
	var keepExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM messages WHERE id = ? AND chat_id = ?
	`, keepMessageID, chatID).Scan(&keepExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatCompaction{}, fmt.Errorf("create chat compaction: kept message not found")
		}
		return ChatCompaction{}, fmt.Errorf("check compaction kept message: %w", err)
	}
	var lastMessageID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE chat_id = ?`, chatID).Scan(&lastMessageID); err != nil {
		return ChatCompaction{}, fmt.Errorf("find compaction last message: %w", err)
	}
	providerValue, err := chatProviderValue(ctx, tx, userID, providerID)
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("create chat compaction: %w", err)
	}
	var modelValue any
	if model != "" {
		modelValue = model
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO chat_compactions (chat_id, provider_id, model, keep_message_id, last_message_id, summary)
		VALUES (?, ?, ?, ?, ?, 'pending')
	`, chatID, providerValue, modelValue, keepMessageID, lastMessageID)
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("insert chat compaction: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("get chat compaction ID: %w", err)
	}
	sealed, err := EncryptStorageText(ctx, chatCompactionSummaryScope, strconv.FormatInt(id, 10), summary)
	if err != nil {
		return ChatCompaction{}, fmt.Errorf("encrypt chat compaction summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_compactions SET summary = ? WHERE id = ?`, sealed, id); err != nil {
		return ChatCompaction{}, fmt.Errorf("store chat compaction summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ChatCompaction{}, fmt.Errorf("commit chat compaction: %w", err)
	}
	compaction := ChatCompaction{
		ID:            id,
		ChatID:        chatID,
		Model:         model,
		KeepMessageID: keepMessageID,
		LastMessageID: lastMessageID,
		Summary:       summary,
	}
	if providerValue != nil {
		compaction.ProviderID = providerID
	}
	return compaction, nil
}

// GetLatestChatCompaction returns the newest summary whose kept message still
// exists. A deleted kept message invalidates that checkpoint; an older valid
// checkpoint may still describe the conversation.
func GetLatestChatCompaction(ctx context.Context, database *sql.DB, chatID int64) (ChatCompaction, bool, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return ChatCompaction{}, false, fmt.Errorf("get chat compaction: %w", err)
	}
	var compaction ChatCompaction
	var providerID sql.NullInt64
	var model, sealed sql.NullString
	err = database.QueryRowContext(ctx, `
		SELECT compactions.id, compactions.chat_id, compactions.provider_id,
			compactions.model, compactions.keep_message_id, compactions.last_message_id, compactions.summary
		FROM chat_compactions AS compactions
		JOIN chats ON chats.id = compactions.chat_id
		WHERE compactions.chat_id = ?
			AND chats.user_id = ?
			AND compactions.keep_message_id IS NOT NULL
		ORDER BY compactions.id DESC
		LIMIT 1
	`, chatID, userID).Scan(
		&compaction.ID, &compaction.ChatID, &providerID,
		&model, &compaction.KeepMessageID, &compaction.LastMessageID, &sealed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if err := database.QueryRowContext(ctx, `SELECT 1 FROM chats WHERE id = ? AND user_id = ?`, chatID, userID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ChatCompaction{}, false, ErrChatNotFound
			}
			return ChatCompaction{}, false, fmt.Errorf("check chat compaction chat: %w", err)
		}
		return ChatCompaction{}, false, nil
	}
	if err != nil {
		return ChatCompaction{}, false, fmt.Errorf("get chat compaction: %w", err)
	}
	compaction.ProviderID = nullSettingInt64(providerID)
	compaction.Model = nullSettingString(model)
	compaction.Summary, err = DecryptStorageText(ctx, chatCompactionSummaryScope,
		strconv.FormatInt(compaction.ID, 10), sealed.String)
	if err != nil {
		return ChatCompaction{}, false, fmt.Errorf("decrypt chat compaction summary: %w", err)
	}
	return compaction, true, nil
}
