package app

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
)

func newEncryptedChatHTTPContext(t *testing.T) context.Context {
	t.Helper()

	key := make([]byte, kritui_db.StorageKeySize)
	for i := range key {
		key[i] = byte(i + 11)
	}
	storage, err := kritui_db.NewStorageCipher(key)
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	ctx := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	return kritui_db.WithStorageCipher(ctx, storage)
}

func seedEncryptedChatTitle(t *testing.T, database *sql.DB, ctx context.Context, title string) int64 {
	t.Helper()

	chatID := seedChat(t, database, "", nil, nil)
	sealed, err := kritui_db.EncryptStorageText(ctx, "chats.title", strconv.FormatInt(chatID, 10), title)
	if err != nil {
		t.Fatalf("EncryptStorageText() error: %v", err)
	}
	if _, err := database.Exec(`UPDATE chats SET title = ? WHERE id = ?`, sealed, chatID); err != nil {
		t.Fatalf("store encrypted title: %v", err)
	}
	return chatID
}

func TestEncryptedChatSummaryDecryptsTitle(t *testing.T) {
	database := openTestDatabase(t)
	ctx := newEncryptedChatHTTPContext(t)
	chatID := seedEncryptedChatTitle(t, database, ctx, "Encrypted title")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /chats/{chat}/summary", chatSummaryHandler(database))
	request := httptest.NewRequest(http.MethodGet, "/chats/"+strconv.FormatInt(chatID, 10)+"/summary", nil).WithContext(ctx)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	requireContains(t, response.Body.String(), "Encrypted title")
	requireNotContains(t, response.Body.String(), "enc:v1:")
}

func TestEncryptedChatSummaryKeepsEmptyTitleSentinel(t *testing.T) {
	database := openTestDatabase(t)
	ctx := newEncryptedChatHTTPContext(t)
	chatID := seedChat(t, database, "", nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /chats/{chat}/summary", chatSummaryHandler(database))
	request := httptest.NewRequest(http.MethodGet, "/chats/"+strconv.FormatInt(chatID, 10)+"/summary", nil).WithContext(ctx)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "New chat")
}

func TestEncryptedChatRenameRoundTrip(t *testing.T) {
	database := openTestDatabase(t)
	ctx := newEncryptedChatHTTPContext(t)
	chatID := seedChat(t, database, "Old title", nil, nil)
	chat := strconv.FormatInt(chatID, 10)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	request := httptest.NewRequest(http.MethodPut, "/chats/"+chat+"?current="+chat, strings.NewReader(url.Values{"title": {"Secret rename"}}.Encode())).WithContext(ctx)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	var stored string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = ?`, chatID).Scan(&stored); err != nil {
		t.Fatalf("query title: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("stored title = %q, want encrypted envelope", stored)
	}
	decrypted, err := kritui_db.DecryptStorageText(ctx, "chats.title", chat, stored)
	if err != nil {
		t.Fatalf("DecryptStorageText() error: %v", err)
	}
	if decrypted != "Secret rename" {
		t.Errorf("decrypted title = %q, want %q", decrypted, "Secret rename")
	}

	summaryMux := http.NewServeMux()
	summaryMux.HandleFunc("GET /chats/{chat}/summary", chatSummaryHandler(database))
	summary := httptest.NewRecorder()
	summaryMux.ServeHTTP(summary, httptest.NewRequest(http.MethodGet, "/chats/"+chat+"/summary", nil).WithContext(ctx))
	if summary.Code != http.StatusOK {
		t.Fatalf("summary status = %d, want %d; body = %q", summary.Code, http.StatusOK, summary.Body.String())
	}
	requireContains(t, summary.Body.String(), "Secret rename")
}

func TestEncryptedChatSummaryRejectsTamperedTitle(t *testing.T) {
	database := openTestDatabase(t)
	ctx := newEncryptedChatHTTPContext(t)
	chatID := seedEncryptedChatTitle(t, database, ctx, "Tamper target")

	var stored string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = ?`, chatID).Scan(&stored); err != nil {
		t.Fatalf("query title: %v", err)
	}
	const prefix = "enc:v1:"
	if !strings.HasPrefix(stored, prefix) {
		t.Fatalf("stored title = %q, want encrypted envelope", stored)
	}
	body := []byte(stored[len(prefix):])
	replacement := byte('A')
	if body[0] == replacement {
		replacement = 'B'
	}
	body[0] = replacement
	tampered := prefix + string(body)
	if _, err := database.Exec(`UPDATE chats SET title = ? WHERE id = ?`, tampered, chatID); err != nil {
		t.Fatalf("store tampered title: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /chats/{chat}/summary", chatSummaryHandler(database))
	request := httptest.NewRequest(http.MethodGet, "/chats/"+strconv.FormatInt(chatID, 10)+"/summary", nil).WithContext(ctx)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Failed to load chat summary.")
	requireNotContains(t, response.Body.String(), "Tamper target")
	requireNotContains(t, response.Body.String(), "enc:v1:")
}
