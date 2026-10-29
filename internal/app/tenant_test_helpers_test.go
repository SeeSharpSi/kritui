package app

import (
	"database/sql"
	"testing"
)

// seedCurrentChat ensures a valid owner-scoped chat exists for request fixtures
// that exercise normal authenticated flows. Missing-chat and foreign-chat tests
// must not call this helper.
func seedCurrentChat(t *testing.T, database *sql.DB, chatID int64) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, chatID); err != nil {
		t.Fatalf("seed current chat %d: %v", chatID, err)
	}
}
