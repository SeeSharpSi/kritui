package kritui_db

import (
	"context"
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

// GetMessages returns a chat's messages in conversation order.
func GetMessages(ctx context.Context, db *sql.DB, chatID int64) ([]llm.Message, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT role, content, model, total_tokens, cost, tool_calls, tool_call_id, provider_metadata
		FROM messages
		WHERE chat_id = ?
		ORDER BY position
	`, chatID)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()

	var messages []llm.Message
	for rows.Next() {
		var message llm.Message
		var model sql.NullString
		var totalTokens sql.NullInt64
		var cost sql.NullFloat64
		var toolCalls sql.NullString
		var toolCallID sql.NullString
		var providerMetadata sql.NullString
		if err := rows.Scan(&message.Role, &message.Content, &model, &totalTokens, &cost, &toolCalls, &toolCallID, &providerMetadata); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}

		if model.Valid {
			message.Model = model.String
		}
		if totalTokens.Valid {
			tokens := int(totalTokens.Int64)
			message.TotalTokens = &tokens
		}
		if cost.Valid {
			value := cost.Float64
			message.Cost = &value
		}
		if toolCalls.Valid {
			if err := json.Unmarshal([]byte(toolCalls.String), &message.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode message tool calls: %w", err)
			}
		}
		if toolCallID.Valid {
			message.ToolCallID = toolCallID.String
		}
		if providerMetadata.Valid {
			if message.Role != "assistant" {
				return nil, fmt.Errorf("decode provider metadata: only assistant messages may contain provider metadata")
			}
			if err := json.Unmarshal([]byte(providerMetadata.String), &message.ProviderMetadata); err != nil {
				return nil, fmt.Errorf("decode provider metadata: %w", err)
			}
		}

		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
}
