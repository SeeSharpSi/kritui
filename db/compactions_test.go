package kritui_db

import (
	"context"
	"testing"
)

func TestChatCompactionRoundTripAndDefaultAutoSetting(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	result, err := database.Exec(`
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'First question'),
			(1, 1, 'assistant', 'First answer'),
			(1, 2, 'user', 'Second question')
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = result
	var keepID, lastID int64
	if err := database.QueryRow(`SELECT id FROM messages WHERE chat_id = 1 AND position = 2`).Scan(&keepID); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT MAX(id) FROM messages WHERE chat_id = 1`).Scan(&lastID); err != nil {
		t.Fatal(err)
	}

	enabled, err := GetCompactionAuto(context.Background(), database)
	if err != nil {
		t.Fatalf("get default automatic compaction: %v", err)
	}
	if !enabled {
		t.Fatal("automatic compaction default = false, want true")
	}
	disabled := false
	if err := SaveSettings(context.Background(), database, SettingsUpdate{Model: "test-model", MaxToolRounds: 16, CompactionAuto: &disabled}); err != nil {
		t.Fatalf("disable automatic compaction: %v", err)
	}
	enabled, err = GetCompactionAuto(context.Background(), database)
	if err != nil || enabled {
		t.Fatalf("automatic compaction after disable = %v, %v", enabled, err)
	}

	created, err := CreateChatCompaction(context.Background(), database, 1, 0, "test-model", "## Objective\nKeep going.", keepID)
	if err != nil {
		t.Fatalf("create compaction: %v", err)
	}
	if created.KeepMessageID != keepID || created.LastMessageID != lastID {
		t.Fatalf("created compaction = %#v, want keep %d last %d", created, keepID, lastID)
	}
	latest, found, err := GetLatestChatCompaction(context.Background(), database, 1)
	if err != nil || !found {
		t.Fatalf("get latest compaction = %#v, %v, %v", latest, found, err)
	}
	if latest.Summary != "## Objective\nKeep going." || latest.KeepMessageID != keepID || latest.Model != "test-model" {
		t.Errorf("latest compaction = %#v", latest)
	}
}

func TestChatCompactionRejectsForeignRows(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	if _, err := database.Exec(`
		INSERT INTO users (id, username) VALUES (2, 'Other');
		INSERT INTO chats (id, user_id) VALUES (1, 1), (2, 2);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'One'),
			(2, 0, 'user', 'Two');
	`); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	if err := database.QueryRow(`SELECT id FROM messages WHERE chat_id = 2`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateChatCompaction(context.Background(), database, 1, 0, "", "summary", messageID); err == nil {
		t.Fatal("CreateChatCompaction accepted a message from another chat")
	}
}
