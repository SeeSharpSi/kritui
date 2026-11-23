package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func TestChatStateSnapshotIsOwnedReadOnlyAndRendersTerminalReply(t *testing.T) {
	database := openTestDatabase(t)
	ctx := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	chatID := seedChat(t, database, "Snapshot", nil, nil)
	for position, message := range []llm.Message{
		{Role: "user", Content: "Question"},
		{Role: "assistant", Content: "Saved answer", Model: "saved-model"},
	} {
		if _, err := kritui_db.InsertMessage(ctx, database, chatID, position, message); err != nil {
			t.Fatal(err)
		}
	}
	chat := strconv.FormatInt(chatID, 10)
	store := newToolCallStore()
	handler := chatStateHandler(database, store)
	request := httptest.NewRequest(http.MethodGet, "/chats/"+chat+"/state", nil).WithContext(ctx)
	request.SetPathValue("chat", chat)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("state status = %d, Cache-Control = %q", response.Code, response.Header().Get("Cache-Control"))
	}
	body := response.Body.String()
	requireContains(t, body,
		`<hx-partial hx-target="#messages" hx-swap="innerHTML">`,
		`<div id="message-history">`,
		`<hx-partial hx-target="#prompt-inbox" hx-swap="outerHTML">`,
		"Question", "Saved answer")
	assertSnapshotSibling(t, body, `</hx-partial>`)
	requireNotContains(t, body, "completion-error", "completion-active")
	if count := strings.Count(body, "<hx-partial"); count != 2 {
		t.Fatalf("snapshot has %d partials, want 2", count)
	}
	if _, active := store.activeForUser(kritui_db.LegacyOwnerID, chatID); active {
		t.Fatal("read-only snapshot allocated a completion tracker")
	}
	var chatCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chatCount); err != nil {
		t.Fatal(err)
	}
	if chatCount != 1 {
		t.Fatalf("snapshot changed chat count to %d", chatCount)
	}

	foreignEncoded, err := hashPassword("foreign-password")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := kritui_db.CreateUser(context.Background(), database, "stateforeign", foreignEncoded)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/chats/"+chat+"/state", nil).
		WithContext(kritui_db.WithUser(context.Background(), foreign.ID))
	request.SetPathValue("chat", chat)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign state status = %d, want 404", response.Code)
	}
	requireNotContains(t, response.Body.String(), "Saved answer", "Question")
}

func TestChatStateSnapshotRendersActiveAndInterruptedCompletion(t *testing.T) {
	database := openTestDatabase(t)
	ctx := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	chatID := seedChat(t, database, "Snapshot", []string{"webfetch"}, nil)
	if _, err := kritui_db.InsertMessage(ctx, database, chatID, 0, llm.Message{Role: "user", Content: "Question"}); err != nil {
		t.Fatal(err)
	}
	chat := strconv.FormatInt(chatID, 10)
	requestFor := func(store *toolCallStore) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/chats/"+chat+"/state", nil).WithContext(ctx)
		request.SetPathValue("chat", chat)
		response := httptest.NewRecorder()
		chatStateHandler(database, store).ServeHTTP(response, request)
		return response
	}

	store := newToolCallStore()
	if _, err := store.createForUser(kritui_db.LegacyOwnerID, chatID, "live-model", []string{"webfetch"}); err != nil {
		t.Fatal(err)
	}
	active := requestFor(store)
	if active.Code != http.StatusOK {
		t.Fatalf("active state status = %d", active.Code)
	}
	requireContains(t, active.Body.String(), "completion-active", "live-model", "Question")
	assertSnapshotSibling(t, active.Body.String(), `<article class="message loading-message completion-active"`)
	requireNotContains(t, active.Body.String(), `hx-trigger="load"`)

	if err := kritui_db.SetCompletionState(ctx, database, chatID, kritui_db.CompletionState{
		Pending: true, Model: "retry-model", Tools: []string{"webfetch"},
	}); err != nil {
		t.Fatal(err)
	}
	interrupted := requestFor(newToolCallStore())
	if interrupted.Code != http.StatusOK {
		t.Fatalf("interrupted state status = %d", interrupted.Code)
	}
	requireContains(t, interrupted.Body.String(), "completion-error", "Completion was interrupted", "retry-model", "Retry completion")
	assertSnapshotSibling(t, interrupted.Body.String(), `<article class="message completion-error"`)
	requireNotContains(t, interrupted.Body.String(), `hx-trigger="load"`)
}

func assertSnapshotSibling(t *testing.T, body, sibling string) {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)<hx-partial hx-target="#messages" hx-swap="innerHTML">\s*<div id="message-history">.*?</div>\s*` + regexp.QuoteMeta(sibling))
	if !pattern.MatchString(body) {
		t.Fatalf("snapshot does not place %q as sibling after #message-history: %s", sibling, body)
	}
}
