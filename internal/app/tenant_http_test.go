package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
)

func TestToolCallStoreScopesCompletionByOwner(t *testing.T) {
	store := newToolCallStore()

	requestID, err := store.createForUser(2, 10, "model", []string{"webfetch"})
	if err != nil {
		t.Fatalf("createForUser() error: %v", err)
	}
	defer store.delete(requestID)

	if _, ok := store.getForUser(1, requestID); ok {
		t.Fatal("foreign owner read a completion tracker")
	}
	if _, ok := store.claimForUser(1, requestID, 10, "model", nil); ok {
		t.Fatal("foreign owner claimed a completion tracker")
	}
	if _, ok := store.activeForUser(1, 10); ok {
		t.Fatal("foreign owner observed an active completion")
	}

	// A different account may use the same chat ID without colliding.
	otherID, err := store.createForUser(1, 10, "other", nil)
	if err != nil {
		t.Fatalf("createForUser(other owner) error: %v", err)
	}
	defer store.delete(otherID)

	tracker, ok := store.claimForUser(2, requestID, 10, "model", []string{"webfetch"})
	if !ok {
		t.Fatal("owning account could not claim its completion tracker")
	}
	if tracker == nil {
		t.Fatal("claimForUser() returned a nil tracker")
	}
	active, ok := store.activeForUser(2, 10)
	if !ok || active.requestID != requestID {
		t.Fatalf("activeForUser() = %#v, %v; want own request", active, ok)
	}
}

func TestToolCallStoreRejectsInvalidOwner(t *testing.T) {
	store := newToolCallStore()

	if _, err := store.createForUser(0, 1, "", nil); !errors.Is(err, errInvalidToolCallOwner) {
		t.Fatalf("createForUser(invalid) error = %v, want errInvalidToolCallOwner", err)
	}
	if _, ok := store.claimForUser(0, "missing", 1, "", nil); ok {
		t.Fatal("claimForUser(invalid) unexpectedly succeeded")
	}
	if _, ok := store.getForUser(0, "missing"); ok {
		t.Fatal("getForUser(invalid) unexpectedly succeeded")
	}
	if _, ok := store.activeForUser(0, 1); ok {
		t.Fatal("activeForUser(invalid) unexpectedly succeeded")
	}
}

func TestForeignChatRequestsReturnNotFoundWithoutChatData(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", "")

	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	chatID, err := kritui_db.AllocateChat(ownerContext, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate owner chat: %v", err)
	}
	if _, err := database.Exec(`UPDATE chats SET title = 'Secret title' WHERE id = ?`, chatID); err != nil {
		t.Fatalf("seed secret title: %v", err)
	}
	encoded, err := hashPassword("foreign-password")
	if err != nil {
		t.Fatalf("hash foreign password: %v", err)
	}
	user, err := kritui_db.CreateUser(context.Background(), database, "foreignuser", encoded)
	if err != nil {
		t.Fatalf("create foreign user: %v", err)
	}
	foreignContext := kritui_db.WithUser(context.Background(), user.ID)
	chat := strconv.FormatInt(chatID, 10)

	check := func(name string, handler http.Handler, request *http.Request) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request.WithContext(foreignContext))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d; body = %q", name, response.Code, http.StatusNotFound, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "Secret title") {
			t.Errorf("%s leaked the owner's chat title", name)
		}
	}

	toolRegistry := newTestToolRegistry(t)
	commandRegistry := newTestCommandRegistry(t, database)
	toolCalls := newToolCallStore()

	check("home", homeHandler(database, toolRegistry, commandRegistry, toolCalls),
		httptest.NewRequest(http.MethodGet, "/?chat="+chat, nil))
	check("history", historyHandler(database),
		httptest.NewRequest(http.MethodGet, "/history?chat="+chat, nil))
	check("settings", settingsHandler(database, toolRegistry),
		httptest.NewRequest(http.MethodGet, "/settings?chat="+chat, nil))
	summaryRequest := httptest.NewRequest(http.MethodGet, "/chats/"+chat+"/summary", nil)
	summaryRequest.SetPathValue("chat", chat)
	check("summary", chatSummaryHandler(database), summaryRequest)

	form := url.Values{"message": {"Hello"}, "model": {"test-model"}}
	request := httptest.NewRequest(http.MethodPost, "/messages?chat="+chat, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("message", messageHandler(database, toolRegistry, commandRegistry, toolCalls), request)

	request = httptest.NewRequest(http.MethodPost, "/messages/retry?chat="+chat, strings.NewReader(url.Values{"model": {"test-model"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("retry", messageRetryHandler(database, toolRegistry, toolCalls), request)

	request = httptest.NewRequest(http.MethodPost, "/messages/complete?chat="+chat, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("completion", messageCompletionHandler(database, toolRegistry, toolCalls, nil), request)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	mux.HandleFunc("PUT /chats/{chat}/pin", pinChatHandler(database))
	mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
	mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, toolRegistry, toolCalls))

	request = httptest.NewRequest(http.MethodPut, "/chats/"+chat+"?current="+chat, strings.NewReader("title=Stolen"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("rename", mux, request)
	request = httptest.NewRequest(http.MethodPut, "/chats/"+chat+"/pin?current="+chat, strings.NewReader("pinned=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("pin", mux, request)
	check("delete", mux, httptest.NewRequest(http.MethodDelete, "/chats/"+chat+"?current="+chat, nil))

	request = httptest.NewRequest(http.MethodPut, "/chats/"+chat+"/messages/1", strings.NewReader("message=Stolen"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	check("edit", mux, request)
}
