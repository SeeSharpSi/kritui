package app

import (
	"context"
	"database/sql"
	"fmt"

	kritui_db "seesharpsi/kritui/db"
)

// requireOwnedChat reports whether chatID is visible to the user carried by
// ctx. Handlers call it before loading or mutating chat-specific data so a
// foreign or missing chat can return the same 404 response without revealing
// whether the row exists for another account.
func requireOwnedChat(ctx context.Context, database *sql.DB, chatID int64) (bool, error) {
	owned, err := kritui_db.OwnsChat(ctx, database, chatID)
	if err != nil {
		return false, fmt.Errorf("check chat ownership: %w", err)
	}
	return owned, nil
}
