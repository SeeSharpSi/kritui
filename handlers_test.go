package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/tools"
)

func TestHomeHandlerRedirectsToNextChatID(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (2, ''), (5, '')`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?view=compact", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(response, request)

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

	homeHandler(database, newTestToolRegistry(t))(response, request)

	if location := response.Header().Get("Location"); location != "/?chat=1" {
		t.Errorf("Location = %q, want %q", location, "/?chat=1")
	}
}

func TestHomeHandlerRendersStoredMessages(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO messages (chat_id, position, role, content, model) VALUES
			(8, 0, 'user', 'Earlier question', NULL),
			(8, 1, 'assistant', 'Earlier answer', 'stored-model');
	`); err != nil {
		t.Fatalf("insert chat history: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("unexpected Location header %q", location)
	}
	requireContains(t, response.Body.String(),
		"Earlier question",
		"Earlier answer",
		`/static/htmx-ext-sse.min.js`,
		`/static/app.js`,
		`reportValidityOfForms`,
		`hx-history="false"`,
		`hx-sync="#messages:drop"`,
		`hx-sync="this:replace"`,
		"<strong>stored-model</strong>",
	)
	requireNotContains(t, response.Body.String(), "What would you like to discuss?", "begin a convo...", "<strong>assistant</strong>")
}

func TestHomeHandlerRendersEmptyChatPrompt(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(), "begin a convo...")
	requireNotContains(t, response.Body.String(), "What would you like to discuss?")
}

func TestHomeHandlerRendersSettingsButtonBeforeHistory(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	settings := strings.Index(body, `id="settings-button"`)
	history := strings.Index(body, `id="history-button"`)
	if settings == -1 || history == -1 || settings > history {
		t.Fatalf("settings button index = %d, history button index = %d; want settings before history", settings, history)
	}
	requireContains(t, body,
		`class="settings-button"`,
		`hx-get="/settings?chat=8"`,
		`aria-label="Settings"`,
		`<svg`,
	)
}

func TestHomeHandlerRendersEndpointModels(t *testing.T) {
	database := openTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "model-a")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	request := httptest.NewRequest(http.MethodGet, "/?chat=1", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`value="model-a"`, `value="model-b"`,
		`name="tool" value="webfetch"`, `name="tool" value="websearch"`,
	)
}

func TestHomeHandlerUsesStoredDefaultModel(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	if err := kritui_db.SetDefaultModel(context.Background(), database, "stored-default"); err != nil {
		t.Fatalf("set default model: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?chat=1", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(), "<strong>stored-default</strong>", `value="stored-default"`)
	requireNotContains(t, response.Body.String(), "env-model")
}

func TestHomeHandlerUsesMostRecentResponseModel(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "default-model")
	t.Setenv("LLM_ENDPOINT", "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO messages (chat_id, position, role, content, model) VALUES
			(8, 0, 'user', 'First question', NULL),
			(8, 1, 'assistant', 'First answer', 'earlier-model'),
			(8, 2, 'user', 'Second question', NULL),
			(8, 3, 'assistant', 'Second answer', 'latest-model'),
			(8, 4, 'user', 'Unanswered question', NULL);
	`); err != nil {
		t.Fatalf("insert chat history: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(), "<strong>latest-model</strong>", `value="latest-model" form="message-form" checked`)
	requireNotContains(t, response.Body.String(), "<strong>default-model</strong>", `value="earlier-model" form="message-form" checked`)
}

func TestHomeHandlerChecksStoredChatTools(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, tools) VALUES (8, ?)`, `["websearch"]`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")

	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	requireContains(t, body, `name="tool" value="websearch" form="message-form" checked`)
	requireNotContains(t, body, `name="tool" value="webfetch" form="message-form" checked`)
}

func TestMessageHandlerPersistsChatToolsAndUserMessage(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "test-model")

	toolCalls := newToolCallStore()
	form := url.Values{
		"message": {"Hello"},
		"model":   {"selected-model"},
		"tool":    {"webfetch", "websearch"},
	}
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), toolCalls), "/messages?chat=3", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	var tools string
	if err := database.QueryRow(`SELECT tools FROM chats WHERE id = 3`).Scan(&tools); err != nil {
		t.Fatalf("query chat tools: %v", err)
	}
	var names []string
	if err := json.Unmarshal([]byte(tools), &names); err != nil {
		t.Fatalf("decode chat tools: %v", err)
	}
	if len(names) != 2 || names[0] != "webfetch" || names[1] != "websearch" {
		t.Fatalf("tools = %#v, want [webfetch websearch]", names)
	}

	var role, content string
	if err := database.QueryRow(`SELECT role, content FROM messages WHERE chat_id = 3`).Scan(&role, &content); err != nil {
		t.Fatalf("query accepted message: %v", err)
	}
	if role != "user" || content != "Hello" {
		t.Errorf("accepted message = %q %q, want user Hello", role, content)
	}
}

func TestHistoryHandlerRendersDeleteButton(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (8, 'Project notes')`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/history?chat=8", nil)
	response := httptest.NewRecorder()

	historyHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`class="history-action history-edit"`,
		`class="history-action history-cancel"`,
		`class="history-action history-delete"`,
		`hx-put="/chats/8?current=8"`,
		`hx-delete="/chats/8?current=8"`,
		`hx-confirm="Permanently delete Project notes?"`,
		`aria-label="Delete Project notes"`,
		`name="title"`,
		`value="Project notes"`,
		`hx-boost="true"`,
		`hx-target="main"`,
		`hx-select="main"`,
		`hx-swap="outerHTML show:none"`,
	)
	requireNotContains(t, response.Body.String(), "<style>", `id="send-button"`)
}

func TestHistoryHandlerClosesFreshChatWithoutRedirect(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/history?chat=8&close=1", nil)
	response := httptest.NewRecorder()

	historyHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if redirect := response.Header().Get("HX-Redirect"); redirect != "" {
		t.Errorf("unexpected HX-Redirect %q", redirect)
	}
	requireContains(t, response.Body.String(), `id="history-button"`, `aria-pressed="false"`)
	requireNotContains(t, response.Body.String(), `id="send-button"`)
}

func TestSettingsHandlerRendersSettingsPage(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil)
	response := httptest.NewRecorder()

	settingsHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`class="panel-page settings-page"`,
		`<h1 id="settings-heading">Settings</h1>`,
		`id="settings-button"`,
		`hx-get="/settings?chat=8&amp;close=1"`,
		`aria-pressed="true"`,
		`id="history-button"`,
		`aria-pressed="false"`,
	)
	requireNotContains(t, response.Body.String(), `id="send-button"`)
}

func TestSettingsHandlerStoresDefaultModel(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database), "/settings?chat=8", url.Values{"model": {"saved-model"}})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	model, err := kritui_db.GetDefaultModel(context.Background(), database, "fallback-model")
	if err != nil {
		t.Fatalf("get default model: %v", err)
	}
	if model != "saved-model" {
		t.Errorf("default model = %q, want saved-model", model)
	}
	requireContains(t, response.Body.String(), `value="saved-model" selected`, "Default model saved.")
}

func TestSettingsHandlerClosesFreshChatWithoutRedirect(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8&close=1", nil)
	response := httptest.NewRecorder()

	settingsHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if redirect := response.Header().Get("HX-Redirect"); redirect != "" {
		t.Errorf("unexpected HX-Redirect %q", redirect)
	}
	requireContains(t, response.Body.String(), `id="settings-button"`, `aria-pressed="false"`)
}

func TestSettingsHandlerClosesSettingsPage(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (8)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8&close=1", nil)
	response := httptest.NewRecorder()

	settingsHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`id="settings-button"`,
		`aria-pressed="false"`,
		`hx-swap-oob="outerHTML"`,
	)
	requireNotContains(t, response.Body.String(), `id="send-button"`)
}

func TestRenameChatHandlerUpdatesTitle(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (8, 'Old title'), (9, 'Other')`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	request := httptest.NewRequest(http.MethodPut, "/chats/8?current=9", strings.NewReader("title=New+title"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "New title")
	requireNotContains(t, response.Body.String(), "Old title")
	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 8`).Scan(&title); err != nil {
		t.Fatalf("query title: %v", err)
	}
	if title != "New title" {
		t.Errorf("stored title = %q, want %q", title, "New title")
	}
}

func TestDeleteChatHandlerPermanentlyDeletesChat(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (7, 'Delete me'), (8, 'Keep me');
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(7, 0, 'user', 'Deleted message'),
			(8, 0, 'user', 'Kept message');
	`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
	request := httptest.NewRequest(http.MethodDelete, "/chats/7?current=8", nil)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Keep me")
	requireNotContains(t, response.Body.String(), "Delete me")
	var chats, deletedMessages, keptMessages int
	if err := database.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM chats),
			(SELECT COUNT(*) FROM messages WHERE chat_id = 7),
			(SELECT COUNT(*) FROM messages WHERE chat_id = 8)
	`).Scan(&chats, &deletedMessages, &keptMessages); err != nil {
		t.Fatalf("count stored rows: %v", err)
	}
	if chats != 1 || deletedMessages != 0 || keptMessages != 1 {
		t.Errorf("stored rows = chats %d, deleted messages %d, kept messages %d; want 1, 0, 1", chats, deletedMessages, keptMessages)
	}

	request = httptest.NewRequest(http.MethodDelete, "/chats/8?current=8", nil)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if redirect := response.Header().Get("HX-Redirect"); redirect != "" {
		t.Errorf("unexpected HX-Redirect %q", redirect)
	}
	requireContains(t, response.Body.String(),
		"No saved chats yet.",
		`id="message-list"`,
		`hx-swap-oob="outerHTML"`,
		"begin a convo...",
	)
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chats); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if chats != 0 {
		t.Errorf("chat count = %d, want 0", chats)
	}
}

func TestMessageHandlerRendersPendingSubmission(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "default-model")
	form := url.Values{"message": {"Hello"}, "model": {"selected-model"}, "tool": {"webfetch"}}
	toolCalls := newToolCallStore()
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), toolCalls), "/messages?chat=1", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		"Hello",
		"selected-model",
		`hx-post="/messages/complete?chat=1"`,
		`hx-trigger="load"`,
		`data-swap-errors`,
		`hx-sync="#messages:queue last"`,
		`hx-disabled-elt="#message-form button[type='submit']"`,
		`hx-ext="sse"`,
		`sse-connect="/messages/tools?request=`,
		`sse-close="close"`,
		`sse-swap="tools"`,
		`hx-swap-oob="outerHTML"`,
		`name="request" value="`,
		`name="tool" value="webfetch"`,
	)
	requireNotContains(t, response.Body.String(), "every 200ms", `type="hidden" name="message"`)
}

func TestMessageHandlerUsesStoredDefaultWhenModelIsMissing(t *testing.T) {
	database := openTestDatabase(t)
	if err := kritui_db.SetDefaultModel(context.Background(), database, "stored-default"); err != nil {
		t.Fatalf("set default model: %v", err)
	}
	toolCalls := newToolCallStore()
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), toolCalls), "/messages?chat=1", url.Values{"message": {"Hello"}})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "stored-default", `name="model" value="stored-default"`)
}

func TestMessageHandlerRejectsConcurrentCompletionForSameChat(t *testing.T) {
	database := openTestDatabase(t)
	registry := newTestToolRegistry(t)
	toolCalls := newToolCallStore()
	handler := messageHandler(database, registry, toolCalls)

	submit := func(chatID int, message string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"message": {message}, "model": {"selected-model"}}
		return postForm(t, handler, "/messages?chat="+strconv.Itoa(chatID), form)
	}

	if response := submit(1, "First"); response.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	response := submit(1, "Second")
	if response.Code != http.StatusConflict {
		t.Fatalf("concurrent status = %d, want %d; body = %q", response.Code, http.StatusConflict, response.Body.String())
	}
	requireContains(t, response.Body.String(), "response is already in progress")
	if response := submit(2, "Other chat"); response.Code != http.StatusOK {
		t.Fatalf("other chat status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	var firstChatMessages, secondChatMessages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1`).Scan(&firstChatMessages); err != nil {
		t.Fatalf("count first chat messages: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 2`).Scan(&secondChatMessages); err != nil {
		t.Fatalf("count second chat messages: %v", err)
	}
	if firstChatMessages != 1 || secondChatMessages != 1 {
		t.Errorf("message counts = %d, %d; want 1, 1", firstChatMessages, secondChatMessages)
	}
}

func TestToolCallStoreCreateRemovesExpiredTrackers(t *testing.T) {
	toolCalls := newToolCallStore()
	startedID := newToolCallRequest(t, toolCalls, 1)
	started, ok := toolCalls.claim(startedID, 1)
	if !ok {
		t.Fatal("claim started tracker failed")
	}
	unclaimedID := newToolCallRequest(t, toolCalls, 2)
	unclaimed, ok := toolCalls.get(unclaimedID)
	if !ok {
		t.Fatal("get unclaimed tracker failed")
	}
	activeID := newToolCallRequest(t, toolCalls, 3)

	toolCalls.mu.Lock()
	toolCalls.trackers[startedID].created = time.Now().Add(-toolCallTrackerTTL)
	toolCalls.trackers[unclaimedID].created = time.Now().Add(-toolCallUnclaimedTrackerTTL)
	toolCalls.mu.Unlock()

	if _, err := toolCalls.create(3); !errors.Is(err, errChatCompletionActive) {
		t.Fatalf("create active chat error = %v, want %v", err, errChatCompletionActive)
	}
	if _, ok := toolCalls.get(startedID); ok {
		t.Error("expired started tracker remains stored")
	}
	if _, ok := toolCalls.get(unclaimedID); ok {
		t.Error("expired unclaimed tracker remains stored")
	}
	waitForTestSignal(t, started.done, "expired started tracker close")
	waitForTestSignal(t, unclaimed.done, "expired unclaimed tracker close")
	if _, err := toolCalls.create(1); err != nil {
		t.Fatalf("reuse expired chat: %v", err)
	}
	toolCalls.delete(activeID)
}

func TestMessageToolStreamHandlerPushesToolCallState(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	tracker, ok := toolCalls.claim(requestID, 1)
	if !ok {
		t.Fatal("claim tool-call tracker failed")
	}
	call := llm.ToolCall{
		ID:   "call-1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "websearch",
			Arguments: `{"query":"braille spinner"}`,
		},
	}
	tracker.observe(call, true)

	request := httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil)
	response := newFlushingResponseRecorder()
	streamDone := make(chan struct{})
	go func() {
		messageToolStreamHandler(toolCalls)(response, request)
		close(streamDone)
	}()
	waitForTestSignal(t, response.flushes, "running tool-call event")

	tracker.observe(call, false)
	waitForTestSignal(t, response.flushes, "completed tool-call event")
	toolCalls.delete(requestID)
	waitForTestSignal(t, response.flushes, "tool-call close event")
	waitForTestSignal(t, streamDone, "tool-call stream completion")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", contentType, "text/event-stream")
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cacheControl, "no-cache")
	}

	events := strings.Split(response.Body.String(), "event: tools\n")
	if len(events) != 3 {
		t.Fatalf("tool update event count = %d, want 2; body = %q", len(events)-1, response.Body.String())
	}
	runningEvent := events[1]
	completedEvent, closeEvent, ok := strings.Cut(events[2], "event: close\n")
	if !ok {
		t.Fatalf("response does not contain close event: %s", response.Body.String())
	}

	requireContains(t, runningEvent, "websearch", "braille spinner", `class="braille-spinner"`, "Running tool:")
	requireContains(t, completedEvent, `class="tool-call-complete"`, "Completed tool:")
	requireNotContains(t, completedEvent, `class="braille-spinner"`)
	requireContains(t, closeEvent, "data: \n")
}

func TestMessageCompletionHandlerIncludesEarlierMessages(t *testing.T) {
	database := openTestDatabase(t)
	type completionRequest struct {
		Model    string        `json:"model"`
		Messages []llm.Message `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	requests := make(chan completionRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Model != "selected-model" {
			t.Errorf("model = %q, want selected-model", request.Model)
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "webfetch" {
			t.Errorf("tools = %#v, want only webfetch", request.Tools)
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Remembered."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	handler := messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)
	timezones := []string{"America/New_York", "Invalid/Timezone"}
	for index, message := range []string{"My name is Cassian.", "What is my name?"} {
		insertAcceptedUser(t, database, 1, message)
		form := url.Values{
			"client_timezone": {timezones[index]},
			"model":           {"selected-model"},
			"request":         {newToolCallRequest(t, toolCalls, 1)},
			"tool":            {"webfetch"},
		}
		response := postForm(t, handler, "/messages/complete?chat=1", form)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
		requireContains(t, response.Body.String(), "<strong>response-model</strong>")
		requireNotContains(t, response.Body.String(), "<strong>test-model</strong>", "<strong>assistant</strong>")
	}

	firstRequest := <-requests
	if len(firstRequest.Messages) != 2 || firstRequest.Messages[0].Role != "system" || firstRequest.Messages[0].Content == "" || firstRequest.Messages[1].Content != "My name is Cassian." {
		t.Fatalf("first request messages = %#v, want system prompt and first user message", firstRequest.Messages)
	}
	requireContains(t, firstRequest.Messages[0].Content,
		"Current UTC datetime:",
		"Client datetime:",
		"Client timezone: America/New_York",
	)
	requireNotContains(t, firstRequest.Messages[0].Content, "Client may be in different timezone")
	secondRequest := <-requests
	if len(secondRequest.Messages) != 4 {
		t.Fatalf("second request message count = %d, want 4", len(secondRequest.Messages))
	}
	if secondRequest.Messages[0].Role != "system" {
		t.Errorf("second request system role = %q, want system", secondRequest.Messages[0].Role)
	}
	requireContains(t, secondRequest.Messages[0].Content,
		"Current UTC datetime:",
		"Client may be in different timezone; if giving times, specify that they're in UTC.",
	)
	requireNotContains(t, secondRequest.Messages[0].Content, "Client datetime:", "Client timezone:")
	want := []llm.Message{
		{Role: "user", Content: "My name is Cassian."},
		{Role: "assistant", Content: "Remembered."},
		{Role: "user", Content: "What is my name?"},
	}
	for index := range want {
		message := secondRequest.Messages[index+1]
		if message.Role != want[index].Role || message.Content != want[index].Content {
			t.Errorf("second request message %d = %#v, want %#v", index+1, message, want[index])
		}
	}
}

func TestClientLocation(t *testing.T) {
	location := clientLocation(" America/New_York ")
	if location == nil || location.String() != "America/New_York" {
		t.Fatalf("clientLocation() = %v, want America/New_York", location)
	}
	if location := clientLocation(""); location != nil {
		t.Errorf("clientLocation(empty) = %v, want nil", location)
	}
	if location := clientLocation("Invalid/Timezone"); location != nil {
		t.Errorf("clientLocation(invalid) = %v, want nil", location)
	}
}

func TestMessageCompletionHandlerStoresRawMarkdown(t *testing.T) {
	database := openTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"**Answer**\n\n- one\n- two"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Question")
	form := url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	}
	response := postForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), "/messages/complete?chat=1", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "<strong>Answer</strong>", "<li>one</li>", "<li>two</li>")

	const want = "**Answer**\n\n- one\n- two"
	var stored string
	if err := database.QueryRow(`SELECT content FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&stored); err != nil {
		t.Fatalf("get stored assistant message: %v", err)
	}
	if stored != want {
		t.Errorf("stored content = %q, want raw Markdown %q", stored, want)
	}
}

func TestMessageCompletionHandlerRendersRetryableError(t *testing.T) {
	database := openTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	insertAcceptedUser(t, database, 1, "Question")
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	form := url.Values{
		"model":   {"selected-model"},
		"request": {requestID},
		"tool":    {"webfetch"},
	}
	registry := newTestToolRegistry(t)
	response := postForm(t, messageCompletionHandler(database, registry, toolCalls, nil), "/messages/complete?chat=1", form)

	if response.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusFailedDependency, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		"Model endpoint returned HTTP 500: Internal Server Error.",
		`role="alert"`,
		`hx-post="/messages/retry?chat=1"`,
		`name="model" value="selected-model"`,
		`name="tool" value="webfetch"`,
		`aria-label="Retry completion"`,
	)
	requireNotContains(t, response.Body.String(), "upstream failed")
	if _, ok := toolCalls.get(requestID); ok {
		t.Error("failed completion kept old tracker active")
	}

	retryResponse := postForm(t, messageRetryHandler(database, registry, toolCalls), "/messages/retry?chat=1", url.Values{
		"model": {"selected-model"},
		"tool":  {"webfetch"},
	})

	if retryResponse.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d; body = %q", retryResponse.Code, http.StatusOK, retryResponse.Body.String())
	}
	requireContains(t, retryResponse.Body.String(), `hx-post="/messages/complete?chat=1"`, `sse-connect="/messages/tools?request=`, `hx-trigger="load"`)

	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1`).Scan(&messages); err != nil {
		t.Fatalf("count messages after retry: %v", err)
	}
	if messages != 1 {
		t.Errorf("message count after retry = %d, want persisted user only", messages)
	}
}

func TestMessageCompletionHandlerRendersToolCallLimitError(t *testing.T) {
	database := openTestDatabase(t)
	fetched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fetched content"))
	}))
	defer fetched.Close()

	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber++
		arguments, _ := json.Marshal(map[string]string{"url": fetched.URL})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","tool_calls":[{
				"id":"call-` + strconv.Itoa(requestNumber) + `",
				"type":"function",
				"function":{"name":"webfetch","arguments":` + strconv.Quote(string(arguments)) + `}
			}]},"finish_reason":"tool_calls"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Keep using tools")
	form := url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
		"tool":    {"webfetch"},
	}
	response := postForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), "/messages/complete?chat=1", form)

	if response.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusFailedDependency, response.Body.String())
	}
	requireContains(t, response.Body.String(), "llm: exceeded 16 consecutive tool-call rounds")
	requireNotContains(t, response.Body.String(), "Failed to complete message.")
}

func TestCompletionErrorMessageDoesNotExposeErrorDetails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "transport error",
			err:  errors.New(`llm: send request: Post "https://secret-token@internal.example/v1": connection refused`),
			want: "Failed to complete message.",
		},
		{
			name: "provider error",
			err: &llm.APIError{
				StatusCode: http.StatusBadGateway,
				Message:    "provider secret detail",
				Body:       `{"secret":"provider credential"}`,
			},
			want: "Model endpoint returned HTTP 502: Bad Gateway.",
		},
		{
			name: "unknown provider status",
			err: &llm.APIError{
				StatusCode: 700,
				Message:    "provider secret detail",
			},
			want: "Model endpoint returned an error.",
		},
		{
			name: "tool call limit",
			err:  errors.New("llm: exceeded 16 consecutive tool-call rounds"),
			want: "llm: exceeded 16 consecutive tool-call rounds",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := completionErrorMessage(test.err); got != test.want {
				t.Errorf("completionErrorMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMessageCompletionHandlerRejectsTrackerFromAnotherChat(t *testing.T) {
	database := openTestDatabase(t)
	insertAcceptedUser(t, database, 1, "First chat")
	insertAcceptedUser(t, database, 2, "Second chat")
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	form := url.Values{"model": {"selected-model"}, "request": {requestID}}
	response := postForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), "/messages/complete?chat=2", form)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if _, ok := toolCalls.claim(requestID, 1); !ok {
		t.Error("cross-chat request consumed valid tracker")
	}
}

func TestMessageCompletionHandlerKeepsCompletedToolCallsAboveAnswer(t *testing.T) {
	database := openTestDatabase(t)
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	fetched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchStarted <- struct{}{}
		select {
		case <-releaseFetch:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("fetched content"))
	}))
	defer fetched.Close()

	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber++
		w.Header().Set("Content-Type", "application/json")
		if requestNumber == 1 {
			arguments, _ := json.Marshal(map[string]string{"url": fetched.URL})
			_, _ = w.Write([]byte(`{
				"model":"response-model",
				"choices":[{"message":{"role":"assistant","tool_calls":[{
					"id":"call-1",
					"type":"function",
					"function":{"name":"webfetch","arguments":` + strconv.Quote(string(arguments)) + `}
				}]},"finish_reason":"tool_calls"}]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Final answer."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Use a tool")
	requestID := newToolCallRequest(t, toolCalls, 1)
	form := url.Values{
		"model":   {"selected-model"},
		"request": {requestID},
		"tool":    {"webfetch"},
	}
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	registry := newTestToolRegistry(t)

	completionDone := make(chan struct{})
	go func() {
		messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
		close(completionDone)
	}()
	select {
	case <-fetchStarted:
	case <-time.After(2 * time.Second):
		close(releaseFetch)
		t.Fatal("tool call did not start")
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil)
	statusContext, cancelStatus := context.WithCancel(statusRequest.Context())
	statusRequest = statusRequest.WithContext(statusContext)
	statusResponse := newFlushingResponseRecorder()
	statusDone := make(chan struct{})
	go func() {
		messageToolStreamHandler(toolCalls)(statusResponse, statusRequest)
		close(statusDone)
	}()
	waitForTestSignal(t, statusResponse.flushes, "running tool-call stream event")
	cancelStatus()
	waitForTestSignal(t, statusDone, "tool-call stream cancellation")
	requireContains(t, statusResponse.Body.String(), "webfetch", `class="braille-spinner"`)

	close(releaseFetch)
	select {
	case <-completionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("message completion did not finish")
	}

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	requireContains(t, body, "webfetch", fetched.URL, "Final answer.", `class="tool-call-complete"`)
	requireNotContains(t, body, `class="braille-spinner"`)
	if strings.Index(body, "webfetch") > strings.Index(body, "Final answer.") {
		t.Errorf("tool call does not precede answer: %s", body)
	}

	var storedMessages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1`).Scan(&storedMessages); err != nil {
		t.Fatalf("count stored messages: %v", err)
	}
	if storedMessages != 4 {
		t.Errorf("stored message count = %d, want 4", storedMessages)
	}

	t.Setenv("LLM_ENDPOINT", "")
	reloadRequest := httptest.NewRequest(http.MethodGet, "/?chat=1", nil)
	reloadResponse := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(reloadResponse, reloadRequest)
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("reload status = %d, want %d; body = %q", reloadResponse.Code, http.StatusOK, reloadResponse.Body.String())
	}
	reloaded := reloadResponse.Body.String()
	requireContains(t, reloaded, "webfetch", fetched.URL, "Final answer.", `class="tool-call-complete"`)
	requireNotContains(t, reloaded, "fetched content")
	if strings.Index(reloaded, "webfetch") > strings.Index(reloaded, "Final answer.") {
		t.Errorf("reloaded tool call does not precede answer: %s", reloaded)
	}
}

func TestMigrateDatabaseAddsSettingsTable(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, total_tokens INTEGER, cost REAL) STRICT`); err != nil {
		t.Fatalf("create legacy messages table: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := kritui_db.SetDefaultModel(context.Background(), database, "migrated-model"); err != nil {
		t.Fatalf("store migrated setting: %v", err)
	}
}

func TestDefaultModelPersistsAfterDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := database.Exec(schema); err != nil {
		database.Close()
		t.Fatalf("initialize database: %v", err)
	}
	if err := kritui_db.EnsureDefaultModel(context.Background(), database, "first-model"); err != nil {
		database.Close()
		t.Fatalf("initialize default model: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer database.Close()
	if err := kritui_db.EnsureDefaultModel(context.Background(), database, "second-model"); err != nil {
		t.Fatalf("reinitialize default model: %v", err)
	}
	model, err := kritui_db.GetDefaultModel(context.Background(), database, "fallback-model")
	if err != nil {
		t.Fatalf("get default model: %v", err)
	}
	if model != "first-model" {
		t.Errorf("default model = %q, want first-model", model)
	}
}

func newTestToolRegistry(t *testing.T) *tools.Registry {
	t.Helper()

	registry, err := tools.NewRegistry(
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool("https://search.example"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	return registry
}

func postForm(t *testing.T, handler http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireContains(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Errorf("content does not contain %q: %s", value, content)
		}
	}
}

func requireNotContains(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(content, value) {
			t.Errorf("content unexpectedly contains %q: %s", value, content)
		}
	}
}

type flushingResponseRecorder struct {
	*httptest.ResponseRecorder
	flushes chan struct{}
}

func newFlushingResponseRecorder() *flushingResponseRecorder {
	return &flushingResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushes:          make(chan struct{}, 4),
	}
}

func (r *flushingResponseRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.flushes <- struct{}{}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func newToolCallRequest(t *testing.T, toolCalls *toolCallStore, chatID int64) string {
	t.Helper()

	requestID, err := toolCalls.create(chatID)
	if err != nil {
		t.Fatalf("create tool-call tracker: %v", err)
	}
	return requestID
}

func insertAcceptedUser(t *testing.T, database *sql.DB, chatID int64, content string) {
	t.Helper()

	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, chatID); err != nil {
		t.Fatalf("insert accepted chat: %v", err)
	}
	var position int
	if err := database.QueryRow(`
		SELECT COALESCE(MAX(position) + 1, 0)
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&position); err != nil {
		t.Fatalf("get accepted message position: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO messages (chat_id, position, role, content)
		VALUES (?, ?, 'user', ?)
	`, chatID, position, content); err != nil {
		t.Fatalf("insert accepted message: %v", err)
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
