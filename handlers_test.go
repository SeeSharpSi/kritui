package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHomeHandlerRedirectsToNextChatID(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (2, ''), (5, '')`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?view=compact", nil)
	response := httptest.NewRecorder()
	homeHandler(database)(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if location := response.Header().Get("Location"); location != "/?chat=6&view=compact" {
		t.Errorf("Location = %q, want %q", location, "/?chat=6&view=compact")
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&count); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if count != 2 {
		t.Errorf("chat count = %d, want 2", count)
	}
}

func TestHomeHandlerUsesFirstChatIDForEmptyDatabase(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	homeHandler(database)(response, request)

	if location := response.Header().Get("Location"); location != "/?chat=1" {
		t.Errorf("Location = %q, want %q", location, "/?chat=1")
	}
}

func TestHomeHandlerRendersWhenChatIDIsPresent(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("unexpected Location header %q", location)
	}
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })

	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	return database
}
