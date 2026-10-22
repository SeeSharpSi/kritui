package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type chatOwnerReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type chatAccess uint8

const (
	chatMissing chatAccess = iota
	chatOwned
	chatForeign
)

// chatAccessFor distinguishes a missing chat from one owned by another account
// without leaking foreign chat contents.
func chatAccessFor(ctx context.Context, db chatOwnerReader, chatID int64) (chatAccess, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return chatMissing, err
	}
	var ownerID sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT user_id FROM chats WHERE id = ?`, chatID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return chatMissing, nil
	}
	if err != nil {
		return chatMissing, fmt.Errorf("check chat ownership: %w", err)
	}
	if !ownerID.Valid || ownerID.Int64 != userID {
		return chatForeign, nil
	}
	return chatOwned, nil
}

func requireChatOwned(ctx context.Context, db chatOwnerReader, chatID int64) error {
	access, err := chatAccessFor(ctx, db, chatID)
	if err != nil {
		return err
	}
	if access != chatOwned {
		return ErrChatNotFound
	}
	return nil
}

// OwnsChat reports whether chatID belongs to the explicit user in ctx. It is
// intended for root routing code that must reject foreign chat IDs before
// building a response.
func OwnsChat(ctx context.Context, database *sql.DB, chatID int64) (bool, error) {
	access, err := chatAccessFor(ctx, database, chatID)
	if err != nil {
		return false, fmt.Errorf("owns chat: %w", err)
	}
	return access == chatOwned, nil
}
