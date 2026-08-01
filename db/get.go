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
		SELECT id, title, tools, created_at, updated_at
		FROM chats
		ORDER BY updated_at DESC, id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("get chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var tools string
		if err := rows.Scan(&chat.ID, &chat.Title, &tools, &chat.CreatedAt, &chat.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		chat.Tools, err = decodeToolNames(tools)
		if err != nil {
			return nil, fmt.Errorf("decode chat tools: %w", err)
		}
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
		SELECT role, content, model, total_tokens, cost, tool_calls, tool_call_id
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
		if err := rows.Scan(&message.Role, &message.Content, &model, &totalTokens, &cost, &toolCalls, &toolCallID); err != nil {
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

		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
}
