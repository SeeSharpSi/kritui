package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/tools"
)

func TestHTTPServerConfiguresConnectionTimeouts(t *testing.T) {
	server := newHTTPServer(http.NewServeMux())
	if server.ReadHeaderTimeout != serverReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, serverReadHeaderTimeout)
	}
	if server.IdleTimeout != serverIdleTimeout {
		t.Errorf("IdleTimeout = %s, want %s", server.IdleTimeout, serverIdleTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want zero so SSE can remain open", server.WriteTimeout)
	}
}

func TestServiceWorkerHandlerServesRootScopeScript(t *testing.T) {
	response := httptest.NewRecorder()
	serviceWorkerHandler(staticFiles)(response, httptest.NewRequest(http.MethodGet, "/sw.js", nil))

	if status := response.Code; status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/javascript", got)
	}
	if got := response.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("Service-Worker-Allowed = %q, want /", got)
	}
	if body := response.Body.String(); !strings.Contains(body, "addEventListener('fetch'") {
		t.Errorf("body does not register a fetch handler")
	}
}

func TestHealthHandlerChecksOnlyDatabase(t *testing.T) {
	database := openTestDatabase(t)
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls++
	}))
	defer provider.Close()
	t.Setenv("LLM_ENDPOINT", provider.URL)

	response := httptest.NewRecorder()
	healthHandler(database)(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want plain text", contentType)
	}
	if body := response.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want ok", body)
	}
	if providerCalls != 0 {
		t.Errorf("provider request count = %d, want 0", providerCalls)
	}
}

func TestHealthHandlerReportsDatabaseFailure(t *testing.T) {
	database := openTestDatabase(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	response := httptest.NewRecorder()
	healthHandler(database)(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want plain text", contentType)
	}
	if body := response.Body.String(); body != "unhealthy\n" {
		t.Errorf("body = %q, want unhealthy", body)
	}
}

func TestOpenDatabaseSerializesConcurrentWrites(t *testing.T) {
	database, err := openDatabase(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("initialize database: %v", err)
	}

	if maximum := database.Stats().MaxOpenConnections; maximum != 1 {
		t.Errorf("maximum open connections = %d, want 1", maximum)
	}
	var busyTimeout int64
	if err := database.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if busyTimeout != databaseBusyTimeout.Milliseconds() {
		t.Errorf("busy timeout = %dms, want %dms", busyTimeout, databaseBusyTimeout.Milliseconds())
	}
	var journalMode string
	if err := database.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "delete" {
		t.Errorf("journal mode = %q, want delete", journalMode)
	}

	start := make(chan struct{})
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for chatID := int64(1); chatID <= 2; chatID++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errors <- persistUserMessage(context.Background(), database, chatID, "message", []string{}, nil)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}

	var messageCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Errorf("message count = %d, want 2", messageCount)
	}
}

func TestOpenDatabaseAcceptsRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	database, err := openDatabase("data.db")
	if err != nil {
		t.Fatalf("open relative database: %v", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatalf("ping relative database: %v", err)
	}
}

func TestHomeHandlerAllocatesNextChatID(t *testing.T) {
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
	if count != 3 {
		t.Errorf("chat count = %d, want 3", count)
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

func TestHomeHandlerAllocatesConcurrentChatsWithIsolatedMessages(t *testing.T) {
	database := openTestDatabase(t)
	registry := newTestToolRegistry(t)
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			homeHandler(database, registry)(responses[index], httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	close(start)
	wait.Wait()

	chatIDs := make([]int64, len(responses))
	for index, response := range responses {
		if response.Code != http.StatusSeeOther {
			t.Fatalf("response %d status = %d, want %d", index, response.Code, http.StatusSeeOther)
		}
		location, err := url.Parse(response.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse response %d location: %v", index, err)
		}
		chatIDs[index], _ = strconv.ParseInt(location.Query().Get("chat"), 10, 64)
	}
	if chatIDs[0] == chatIDs[1] || chatIDs[0] == 0 || chatIDs[1] == 0 {
		t.Fatalf("allocated chat IDs = %v, want distinct positive IDs", chatIDs)
	}

	toolCalls := newToolCallStore()
	forms := []url.Values{
		{"message": {"first message"}, "model": {"test-model"}, "tool": {"webfetch"}},
		{"message": {"second message"}, "model": {"test-model"}, "tool": {"websearch"}},
	}
	for index, chatID := range chatIDs {
		response := postForm(t, messageHandler(database, registry, toolCalls), "/messages?chat="+strconv.FormatInt(chatID, 10), forms[index])
		if response.Code != http.StatusOK {
			t.Fatalf("chat %d message status = %d, want %d; body = %q", chatID, response.Code, http.StatusOK, response.Body.String())
		}
	}
	for index, chatID := range chatIDs {
		messages, err := kritui_db.GetMessages(context.Background(), database, chatID)
		if err != nil {
			t.Fatalf("get chat %d messages: %v", chatID, err)
		}
		if len(messages) != 1 || messages[0].Content != forms[index].Get("message") {
			t.Errorf("chat %d messages = %#v, want isolated %q", chatID, messages, forms[index].Get("message"))
		}
		selectedTools, err := kritui_db.GetChatTools(context.Background(), database, chatID)
		if err != nil {
			t.Fatalf("get chat %d tools: %v", chatID, err)
		}
		if len(selectedTools) != 1 || selectedTools[0] != forms[index].Get("tool") {
			t.Errorf("chat %d tools = %v, want %q", chatID, selectedTools, forms[index].Get("tool"))
		}
	}
}

func TestAllocatedChatsStayHiddenAndAbandonedRowsAreRemoved(t *testing.T) {
	database := openTestDatabase(t)
	registry := newTestToolRegistry(t)
	first := httptest.NewRecorder()
	homeHandler(database, registry)(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if _, err := database.Exec(`UPDATE chats SET created_at = '2000-01-01T00:00:00.000Z'`); err != nil {
		t.Fatalf("age abandoned chat: %v", err)
	}

	history := httptest.NewRecorder()
	historyHandler(database)(history, httptest.NewRequest(http.MethodGet, "/history?chat=1", nil))
	requireContains(t, history.Body.String(), "No saved chats yet.")

	second := httptest.NewRecorder()
	homeHandler(database, registry)(second, httptest.NewRequest(http.MethodGet, "/", nil))
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&count); err != nil {
		t.Fatalf("count allocated chats: %v", err)
	}
	if count != 1 {
		t.Errorf("allocated chat count = %d, want one current allocation", count)
	}
	if second.Header().Get("Location") != "/?chat=2" {
		t.Errorf("second allocation location = %q, want /?chat=2", second.Header().Get("Location"))
	}
}

func TestHomeHandlerRendersStoredMessages(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO messages (chat_id, position, role, content, model, tool_calls, tool_call_id) VALUES
			(8, 0, 'user', 'Earlier question', NULL, NULL, NULL),
			(8, 1, 'assistant', '', 'stored-model', '[{"id":"call-1","type":"function","function":{"name":"webfetch","arguments":"{\"url\":\"https://example.com\"}"}}]', NULL),
			(8, 2, 'tool', 'result', NULL, NULL, 'call-1'),
			(8, 3, 'assistant', 'Earlier answer', 'stored-model', NULL, NULL);
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
		`responseHandling`,
		`&#34;[45]..&#34;,&#34;swap&#34;:true,&#34;error&#34;:true`,
		`hx-history="false"`,
		`hx-sync="#messages:drop"`,
		"<strong>stored-model</strong>",
		`class="tool-call-toggle"`,
		`aria-expanded="false"`,
	)
	requireNotContains(t, response.Body.String(), "What would you like to discuss?", "begin a convo...", "<strong>assistant</strong>", `role="button"`)
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

func TestHomeHandlerPreloadsSettingsAndEmptyHistoryShell(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (7, 'Loaded lazily')`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
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
		`data-panel-target="settings-page"`,
		`aria-controls="settings-page"`,
		`aria-expanded="false"`,
		`aria-label="Settings"`,
		`id="settings-page"`,
		`class="panel-page settings-page"`,
		`hx-post="/settings?chat=8"`,
		`hx-target="#settings-page"`,
		`id="history-page"`,
		`class="panel-page history-page"`,
		`id="history-error"`,
		`class="history-status"`,
		`hx-get="/history?chat=8"`,
		`hx-trigger="history-open"`,
		`hx-target="#history-entries"`,
		`hx-sync="this:replace"`,
		`data-panel-initial-focus`,
		`<svg`,
	)
	if count := strings.Count(body, `hx-get="/history?chat=8"`); count != 1 {
		t.Errorf("first-page history request count = %d, want 1", count)
	}
	requireNotContains(t, body, `hx-get="/settings?chat=8"`, "Loaded lazily", "loadHistory")

	script, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app script: %v", err)
	}
	requireContains(t, string(script), `selectedPanel.dispatchEvent(new Event('history-open'))`)
	requireContains(t, string(script), `button.setAttribute('aria-expanded', String(active))`)
	requireContains(t, string(script), `selectedPanel.querySelector('[data-panel-initial-focus]')?.focus()`)
	requireContains(t, string(script), `panelButton?.focus()`)
	requireNotContains(t, string(script), `querySelector('.history-loader')`, "htmx.trigger")
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
	requireContains(t, response.Body.String(),
		"<strong>latest-model</strong>",
		`value="latest-model" form="message-form" checked`,
		`<option value="default-model" selected>default-model</option>`,
	)
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

func TestHomeHandlerRendersPromptAppends(t *testing.T) {
	database := openTestDatabase(t)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/?chat=8", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`class="append-picker"`,
		`>Appends</summary>`,
		`name="append" value="link-check" form="message-form"`,
		`name="append" value="research" form="message-form"`,
		`class="append-state"`,
	)
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

func TestMessageHandlerAppliesAndPersistsPromptAppends(t *testing.T) {
	database := openTestDatabase(t)
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages?chat=3", url.Values{
		"message": {"Hello"},
		"model":   {"selected-model"},
		"append":  {"research", "link-check"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	defaults := kritui_db.DefaultPromptAppends()

	var content, encodedPromptAppendTexts, encodedAppendIDs string
	if err := database.QueryRow(`
		SELECT messages.content, messages.prompt_appends, chats.appends
		FROM messages
		JOIN chats ON chats.id = messages.chat_id
		WHERE messages.chat_id = 3
	`).Scan(&content, &encodedPromptAppendTexts, &encodedAppendIDs); err != nil {
		t.Fatalf("query message and append selections: %v", err)
	}
	if content != "Hello" {
		t.Errorf("stored message = %q, want original Hello", content)
	}
	var storedPromptAppendTexts []string
	if err := json.Unmarshal([]byte(encodedPromptAppendTexts), &storedPromptAppendTexts); err != nil {
		t.Fatalf("decode message prompt append texts: %v", err)
	}
	wantPromptAppendTexts := []string{defaults[1].Text, defaults[0].Text}
	if !slices.Equal(storedPromptAppendTexts, wantPromptAppendTexts) {
		t.Errorf("stored message prompt append texts = %v, want %v", storedPromptAppendTexts, wantPromptAppendTexts)
	}
	storedMessages, err := kritui_db.GetMessages(context.Background(), database, 3)
	if err != nil {
		t.Fatalf("get stored messages: %v", err)
	}
	if len(storedMessages) != 1 || !slices.Equal(storedMessages[0].PromptAppendTexts, wantPromptAppendTexts) {
		t.Errorf("loaded message prompt append texts = %#v, want %v", storedMessages, wantPromptAppendTexts)
	}
	var selected []string
	if err := json.Unmarshal([]byte(encodedAppendIDs), &selected); err != nil {
		t.Fatalf("decode chat prompt append IDs: %v", err)
	}
	if !slices.Equal(selected, []string{"research", "link-check"}) {
		t.Errorf("stored chat prompt append IDs = %v, want [research link-check]", selected)
	}
	body := response.Body.String()
	requireContains(t, body,
		`<div class="message-content plain-text">Hello</div>`,
		`<details class="message-appends">`,
		`<summary>appends</summary>`,
		html.EscapeString(defaults[1].Text),
		html.EscapeString(defaults[0].Text),
	)
	appendIndex := strings.Index(body, `<details class="message-appends">`)
	messageIndex := strings.Index(body, `<article class="message user">`)
	if appendIndex == -1 || messageIndex == -1 || appendIndex >= messageIndex {
		t.Errorf("append details index = %d, user message index = %d; want details before message", appendIndex, messageIndex)
	}
}

func TestMessageHandlerRejectsUnknownPromptAppend(t *testing.T) {
	database := openTestDatabase(t)
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages?chat=3", url.Values{
		"message": {"Hello"},
		"model":   {"selected-model"},
		"append":  {"missing"},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	requireContains(t, response.Body.String(), "Prompt append selection is invalid.")
	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 0 {
		t.Errorf("stored messages = %d, want 0", messages)
	}
}

func TestMessagesWithPromptAppendTextsExpandsCopyForCompletion(t *testing.T) {
	original := []llm.Message{{
		Role:              "user",
		Content:           "Question",
		PromptAppendTexts: []string{"First instruction", "Second instruction"},
	}}

	expanded := messagesWithPromptAppendTexts(original)
	if original[0].Content != "Question" {
		t.Errorf("original message content = %q, want Question", original[0].Content)
	}
	if expanded[0].Content != "Question\n\nFirst instruction\n\nSecond instruction" {
		t.Errorf("expanded message content = %q", expanded[0].Content)
	}
}

func TestChatTitleNormalizationPreservesMessageContent(t *testing.T) {
	database := openTestDatabase(t)
	firstLine := strings.Repeat("界", maxChatTitleRunes+1)
	message := "\n  " + firstLine + "  \nsecond line\n"
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages?chat=3", url.Values{
		"message": {message},
		"model":   {"selected-model"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	var title, content string
	if err := database.QueryRow(`
		SELECT chats.title, messages.content
		FROM chats
		JOIN messages ON messages.chat_id = chats.id
		WHERE chats.id = 3
	`).Scan(&title, &content); err != nil {
		t.Fatalf("query stored title and message: %v", err)
	}
	if title != strings.Repeat("界", maxChatTitleRunes) {
		t.Errorf("stored title = %q, want %d complete runes", title, maxChatTitleRunes)
	}
	if content != strings.TrimSpace(message) {
		t.Errorf("stored message = %q, want untruncated %q", content, strings.TrimSpace(message))
	}
}

func TestNormalizeChatTitle(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "trimmed first line", value: "  First line  \nSecond line", want: "First line"},
		{name: "first useful line", value: "\n \t\nUseful\nLater", want: "Useful"},
		{name: "empty", value: " \n\t ", want: ""},
		{name: "exact Unicode limit", value: strings.Repeat("界", maxChatTitleRunes), want: strings.Repeat("界", maxChatTitleRunes)},
		{name: "truncated Unicode", value: strings.Repeat("界", maxChatTitleRunes+1), want: strings.Repeat("界", maxChatTitleRunes)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeChatTitle(test.value); got != test.want {
				t.Errorf("normalizeChatTitle() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHandlersLimitURLFormBodies(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		base       string
		limit      int64
		newHandler func(*testing.T) http.Handler
		wantHTML   string
	}{
		{
			name:   "message",
			method: http.MethodPost,
			target: "/messages?chat=invalid",
			base:   "message=hello&model=test-model",
			limit:  maxMessageBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				return messageHandler(openTestDatabase(t), newTestToolRegistry(t), newToolCallStore())
			},
			wantHTML: `<article class="message" role="alert">`,
		},
		{
			name:   "completion",
			method: http.MethodPost,
			target: "/messages/complete?chat=1",
			base:   "model=test-model&request=invalid",
			limit:  maxCompletionBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				return messageCompletionHandler(openTestDatabase(t), newTestToolRegistry(t), newToolCallStore(), nil)
			},
			wantHTML: `<article class="message" role="alert">`,
		},
		{
			name:   "retry",
			method: http.MethodPost,
			target: "/messages/retry?chat=1",
			base:   "model=test-model",
			limit:  maxRetryBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				return messageRetryHandler(openTestDatabase(t), newTestToolRegistry(t), newToolCallStore())
			},
			wantHTML: `<article class="message" role="alert">`,
		},
		{
			name:   "settings",
			method: http.MethodPost,
			target: "/settings?chat=1",
			base:   "model=test-model",
			limit:  maxSettingsBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				t.Setenv("LLM_MODEL", "test-model")
				t.Setenv("LLM_ENDPOINT", "")
				return settingsHandler(openTestDatabase(t), newTestToolRegistry(t))
			},
			wantHTML: `id="settings-page"`,
		},
		{
			name:   "rename",
			method: http.MethodPut,
			target: "/chats/1?current=1",
			base:   "title=Renamed",
			limit:  maxRenameBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				database := openTestDatabase(t)
				if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (1, 'Original')`); err != nil {
					t.Fatalf("insert chat: %v", err)
				}
				mux := http.NewServeMux()
				mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
				return mux
			},
			wantHTML: `id="history-error"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := test.newHandler(t)
			atLimit := serveRawForm(handler, test.method, test.target, paddedFormBody(t, test.base, test.limit))
			if atLimit.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("body at limit returned 413: %s", atLimit.Body.String())
			}

			oversized := serveRawForm(handler, test.method, test.target, paddedFormBody(t, test.base, test.limit+1))
			requireHTMLErrorResponse(t, oversized, http.StatusRequestEntityTooLarge, test.wantHTML, "Request body is too large.")
		})
	}
}

func TestMessageHandlerLimitsMultipartBody(t *testing.T) {
	database := openTestDatabase(t)
	handler := messageHandler(database, newTestToolRegistry(t), newToolCallStore())

	body, contentType := multipartBodyOfSize(t, maxMessageBodyBytes)
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=invalid", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart body at limit returned 413: %s", response.Body.String())
	}

	body, contentType = multipartBodyOfSize(t, maxMessageBodyBytes+1)
	request = httptest.NewRequest(http.MethodPost, "/messages?chat=invalid", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response = httptest.NewRecorder()
	handler(response, request)
	requireHTMLErrorResponse(t, response, http.StatusRequestEntityTooLarge, `<article class="message" role="alert">`, "Request body is too large.")
}

func TestOversizedMessageFormsDoNotChangeState(t *testing.T) {
	t.Run("submission", func(t *testing.T) {
		database := openTestDatabase(t)
		toolCalls := newToolCallStore()
		body := paddedFormBody(t, "message=hello&model=test-model", maxMessageBodyBytes+1)
		response := serveRawForm(messageHandler(database, newTestToolRegistry(t), toolCalls), http.MethodPost, "/messages?chat=1", body)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", response.Code)
		}
		var messages int
		if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if messages != 0 {
			t.Errorf("stored message count = %d, want 0", messages)
		}
		requestID, err := toolCalls.create(1)
		if err != nil {
			t.Fatalf("oversized submission retained active chat: %v", err)
		}
		toolCalls.delete(requestID)
	})

	t.Run("completion", func(t *testing.T) {
		database := openTestDatabase(t)
		toolCalls := newToolCallStore()
		requestID := newToolCallRequest(t, toolCalls, 1)
		body := paddedFormBody(t, "model=test-model&request="+requestID, maxCompletionBodyBytes+1)
		response := serveRawForm(messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), http.MethodPost, "/messages/complete?chat=1", body)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", response.Code)
		}
		if _, ok := toolCalls.claim(requestID, 1); !ok {
			t.Fatal("oversized completion consumed tracker")
		}
		toolCalls.delete(requestID)
	})

	t.Run("retry", func(t *testing.T) {
		database := openTestDatabase(t)
		toolCalls := newToolCallStore()
		body := paddedFormBody(t, "model=test-model", maxRetryBodyBytes+1)
		response := serveRawForm(messageRetryHandler(database, newTestToolRegistry(t), toolCalls), http.MethodPost, "/messages/retry?chat=1", body)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", response.Code)
		}
		requestID, err := toolCalls.create(1)
		if err != nil {
			t.Fatalf("oversized retry retained active chat: %v", err)
		}
		toolCalls.delete(requestID)
	})
}

func TestHistoryHandlerRendersDeleteButton(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (8, 'Project notes');
		INSERT INTO messages (chat_id, position, role, content) VALUES (8, 0, 'user', 'notes');
	`); err != nil {
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
		`class="history-save"`,
		`type="submit"`,
		`aria-current="page"`,
		`hx-put="/chats/8?current=8"`,
		`hx-delete="/chats/8?current=8"`,
		`hx-confirm="Permanently delete Project notes?"`,
		`aria-label="Delete Project notes"`,
		`hx-target="#history-entries"`,
		`name="title"`,
		`value="Project notes"`,
		`hx-boost="true"`,
		`hx-target="main"`,
		`hx-select="main"`,
		`hx-swap="outerHTML show:none"`,
		`id="history-error"`,
		`hx-swap-oob="outerHTML"`,
	)
	requireNotContains(t, response.Body.String(), "<style>", `id="send-button"`)
}

func TestHistoryHandlerPaginatesFromNewestToOldest(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title, updated_at) VALUES
			(1, 'Oldest', '2026-01-01T00:00:00.000Z'),
			(2, 'Middle', '2026-01-02T00:00:00.000Z'),
			(3, 'Newest', '2026-01-03T00:00:00.000Z');
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'oldest'),
			(2, 0, 'user', 'middle'),
			(3, 0, 'user', 'newest');
		UPDATE chats SET updated_at = CASE id
			WHEN 1 THEN '2026-01-01T00:00:00.000Z'
			WHEN 2 THEN '2026-01-02T00:00:00.000Z'
			WHEN 3 THEN '2026-01-03T00:00:00.000Z'
		END;
	`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/history?chat=8&limit=2", nil)
	response := httptest.NewRecorder()

	historyHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	requireContains(t, body, "Newest", "Middle", `before_id=2`, `class="history-loader"`, `hx-target="this"`)
	requireNotContains(t, body, "Oldest")

	request = httptest.NewRequest(http.MethodGet, "/history?chat=8&limit=2&before=2026-01-02T00:00:00.000Z&before_id=2", nil)
	response = httptest.NewRecorder()
	historyHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("second page status = %d, want %d", response.Code, http.StatusOK)
	}
	body = response.Body.String()
	requireContains(t, body, "Oldest")
	requireNotContains(t, body, "Newest", "Middle", `class="history-loader"`)
}

func TestHistoryHandlerRefreshesCurrentTitlesAndOrdering(t *testing.T) {
	database := openTestDatabase(t)
	loadHistory := func() string {
		response := httptest.NewRecorder()
		historyHandler(database)(response, httptest.NewRequest(http.MethodGet, "/history?chat=1", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("history status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
		return response.Body.String()
	}

	requireContains(t, loadHistory(), "No saved chats yet.")
	if err := persistUserMessage(context.Background(), database, 1, "First title", []string{}, nil); err != nil {
		t.Fatalf("store first chat: %v", err)
	}
	requireContains(t, loadHistory(), "First title")

	if err := persistUserMessage(context.Background(), database, 2, "Newest title", []string{}, nil); err != nil {
		t.Fatalf("store newest chat: %v", err)
	}
	refreshed := loadHistory()
	newestIndex := strings.Index(refreshed, "Newest title")
	firstIndex := strings.Index(refreshed, "First title")
	if newestIndex == -1 || firstIndex == -1 || newestIndex >= firstIndex {
		t.Errorf("refreshed history ordering is stale: %s", refreshed)
	}
}

func TestSettingsHandlerRendersSettingsPage(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil)
	response := httptest.NewRecorder()

	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(),
		`id="settings-page"`,
		`class="panel-page settings-page"`,
		`id="settings-heading"`,
		`tabindex="-1"`,
		`data-panel-initial-focus`,
		`>Settings</h1>`,
		`hx-target="#settings-page"`,
		`hx-swap="outerHTML"`,
	)
	requireNotContains(t, response.Body.String(), `id="send-button"`, `id="settings-button"`, ` hidden`)
}

func TestSettingsHandlerStoresDefaultModelAndMaxToolRounds(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{"model": {"saved-model"}, "max_tool_rounds": {"32"}})

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
	maxToolRounds, err := kritui_db.GetMaxToolRounds(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get max tool rounds: %v", err)
	}
	if maxToolRounds != 32 {
		t.Errorf("max tool rounds = %d, want 32", maxToolRounds)
	}
	requireContains(t, response.Body.String(), `value="saved-model" selected`, `name="max_tool_rounds"`, `value="32"`)
}

func TestSettingsHandlerRejectsInvalidMaxToolRounds(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	for _, value := range []string{"0", "101", "abc", ""} {
		response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{"model": {"saved-model"}, "max_tool_rounds": {value}})
		if response.Code != http.StatusBadRequest {
			t.Errorf("status for %q = %d, want %d; body = %q", value, response.Code, http.StatusBadRequest, response.Body.String())
			continue
		}
		requireContains(t, response.Body.String(), "Max tool-call rounds")
	}
}

func TestSettingsHandlerStoresDefaultTools(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"default_tool":    {"websearch", "webfetch"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	names, err := kritui_db.GetDefaultEnabledTools(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("get default enabled tools: %v", err)
	}
	if len(names) != 2 || !containsString(names, "webfetch") || !containsString(names, "websearch") {
		t.Errorf("default enabled tools = %v, want webfetch and websearch", names)
	}
	requireContains(t, response.Body.String(),
		`name="default_tool" value="webfetch" checked`,
		`name="default_tool" value="websearch" checked`,
		"Settings saved.")
}

func TestSettingsHandlerStoresPromptAppends(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_form":        {"1"},
		"append_id":          {"custom"},
		"append_name_custom": {"Custom"},
		"append_text_custom": {"Use custom instruction."},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	values, err := kritui_db.GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("get prompt appends: %v", err)
	}
	want := []kritui_db.PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction."}}
	if !slices.Equal(values, want) {
		t.Errorf("prompt appends = %#v, want %#v", values, want)
	}
	requireContains(t, response.Body.String(), `name="append_name_custom"`, `value="Custom"`, "Use custom instruction.", "Settings saved.")
}

func TestSettingsHandlerStoresLargePromptAppend(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	largeText := strings.Repeat("x", 16*1024)
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":             {"saved-model"},
		"max_tool_rounds":   {"16"},
		"append_form":       {"1"},
		"append_id":         {"large"},
		"append_name_large": {"Large"},
		"append_text_large": {largeText},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	values, err := kritui_db.GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("get prompt appends: %v", err)
	}
	want := []kritui_db.PromptAppend{{ID: "large", Name: "Large", Text: largeText}}
	if !slices.Equal(values, want) {
		t.Errorf("prompt appends = %#v, want %#v", values, want)
	}
}

func TestSettingsHandlerRefreshesAppendPickerAfterHTMXSave(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	form := url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_form":        {"1"},
		"append_id":          {"custom"},
		"append_name_custom": {"Custom"},
		"append_text_custom": {"Use custom instruction."},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings?chat=8", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), `id="append-picker"`, `hx-swap-oob="outerHTML"`, `value="custom"`, "Custom")
}

func TestSettingsHandlerReportsChatAppendsLoadFailureAfterHTMXSave(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	if _, err := database.Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("enable ignoring check constraints: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO chats (id, appends) VALUES (8, 'not-json')`); err != nil {
		t.Fatalf("insert corrupt chat appends: %v", err)
	}
	if _, err := database.Exec(`PRAGMA ignore_check_constraints = OFF`); err != nil {
		t.Fatalf("disable ignoring check constraints: %v", err)
	}
	form := url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_form":        {"1"},
		"append_id":          {"custom"},
		"append_name_custom": {"Custom"},
		"append_text_custom": {"Use custom instruction."},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings?chat=8", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Failed to load chat appends.")
	requireNotContains(t, response.Body.String(), `hx-swap-oob="outerHTML"`)
}

func TestSettingsHandlerAppendActionsIgnoreInvalidUnrelatedSettings(t *testing.T) {
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	defaults := kritui_db.DefaultPromptAppends()
	for _, test := range []struct {
		name   string
		action url.Values
		want   []kritui_db.PromptAppend
	}{
		{
			name: "add",
			action: url.Values{
				"model":              {""},
				"max_tool_rounds":    {"not-a-number"},
				"append_form":        {"1"},
				"append_id":          {"custom"},
				"append_name_custom": {"Custom"},
				"append_text_custom": {"Use custom instruction."},
				"append_action":      {"add"},
			},
			want: defaults,
		},
		{
			name: "remove",
			action: url.Values{
				"model":              {""},
				"max_tool_rounds":    {"not-a-number"},
				"append_form":        {"1"},
				"append_id":          {"custom"},
				"append_name_custom": {"Custom"},
				"append_text_custom": {"Use custom instruction."},
				"remove_append":      {"custom"},
			},
			want: defaults,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDatabase(t)
			response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", test.action)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
			}
			requireNotContains(t, response.Body.String(), "A model is required.", "Max tool-call rounds must be between", "Tool selection is invalid.")
			if test.name == "add" {
				requireContains(t, response.Body.String(), "new append", `name="append_name_`)
			}
			if test.name == "remove" {
				requireNotContains(t, response.Body.String(), "append_name_custom")
			}

			values, err := kritui_db.GetPromptAppends(context.Background(), database)
			if err != nil {
				t.Fatalf("get prompt appends: %v", err)
			}
			if !slices.Equal(values, defaults) {
				t.Errorf("prompt appends persisted = %#v, want defaults %#v", values, defaults)
			}
		})
	}
}

func TestSettingsHandlerReportsPromptAppendValidationError(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_form":        {"1"},
		"append_id":          {"custom"},
		"append_name_custom": {""},
		"append_text_custom": {"Use custom instruction."},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), `Prompt append settings are invalid: prompt append &#34;custom&#34; name is required.`)
}

func TestSettingsHandlerReportsPromptAppendFormError(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_form":        {"1"},
		"append_id":          {"custom", "custom"},
		"append_name_custom": {"Custom"},
		"append_text_custom": {"Use custom instruction."},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), `Prompt append form is invalid: prompt append &#34;custom&#34; is duplicated.`)
}

func TestSettingsHandlerStoresEmptyDefaultTools(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	if err := kritui_db.SetDefaultEnabledTools(context.Background(), database, []string{"webfetch"}); err != nil {
		t.Fatalf("set default enabled tools: %v", err)
	}
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	names, err := kritui_db.GetDefaultEnabledTools(context.Background(), database, []string{"fallback"})
	if err != nil {
		t.Fatalf("get default enabled tools: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("default enabled tools = %v, want empty list", names)
	}
	requireNotContains(t, response.Body.String(), `name="default_tool" value="webfetch" checked`)
}

func TestSettingsHandlerRejectsInvalidDefaultTools(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"default_tool":    {"bogus"},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Tool selection is invalid.")
	names, err := kritui_db.GetDefaultEnabledTools(context.Background(), database, []string{"fallback"})
	if err != nil {
		t.Fatalf("get default enabled tools: %v", err)
	}
	if len(names) != 1 || names[0] != "fallback" {
		t.Errorf("default enabled tools = %v, want fallback [fallback]", names)
	}
}

func TestHomeHandlerAllocatesChatWithDefaultTools(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	if err := kritui_db.SetDefaultEnabledTools(context.Background(), database, []string{"websearch"}); err != nil {
		t.Fatalf("set default enabled tools: %v", err)
	}

	redirect := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(redirect, httptest.NewRequest(http.MethodGet, "/", nil))
	if redirect.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", redirect.Code, http.StatusSeeOther)
	}
	location, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	chatID := location.Query().Get("chat")

	names, err := kritui_db.GetChatTools(context.Background(), database, mustChatID(t, chatID))
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
	}
	if len(names) != 1 || names[0] != "websearch" {
		t.Errorf("chat tools = %v, want [websearch]", names)
	}

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(page, httptest.NewRequest(http.MethodGet, "/?chat="+chatID, nil))
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d, want %d; body = %q", page.Code, http.StatusOK, page.Body.String())
	}
	requireContains(t, page.Body.String(), `name="tool" value="websearch" form="message-form" checked`)
	requireNotContains(t, page.Body.String(), `name="tool" value="webfetch" form="message-form" checked`)
}

func TestHomeHandlerAllocatesChatWithoutDefaultToolsWhenUnset(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	redirect := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(redirect, httptest.NewRequest(http.MethodGet, "/", nil))
	if redirect.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", redirect.Code, http.StatusSeeOther)
	}
	location, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	chatID := location.Query().Get("chat")

	names, err := kritui_db.GetChatTools(context.Background(), database, mustChatID(t, chatID))
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("chat tools = %v, want empty list", names)
	}

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(page, httptest.NewRequest(http.MethodGet, "/?chat="+chatID, nil))
	requireNotContains(t, page.Body.String(),
		`name="tool" value="webfetch" form="message-form" checked`,
		`name="tool" value="websearch" form="message-form" checked`,
	)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func mustChatID(t *testing.T, value string) int64 {
	t.Helper()
	chatID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || chatID <= 0 {
		t.Fatalf("invalid chat ID %q", value)
	}
	return chatID
}

func TestHistoryHandlerRejectsInvalidPagination(t *testing.T) {
	database := openTestDatabase(t)
	for _, target := range []string{
		"/history?chat=8&limit=0",
		"/history?chat=8&limit=51",
		"/history?chat=8&before=2026-01-01T00:00:00Z",
		"/history?chat=8&before_id=2",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		historyHandler(database)(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", target, response.Code, http.StatusBadRequest)
		}
	}
}

func TestRenameChatHandlerUpdatesTitle(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (8, 'Old title'), (9, 'Other');
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(8, 0, 'user', 'first'),
			(9, 0, 'user', 'second');
	`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
	request := httptest.NewRequest(http.MethodPut, "/chats/8?current=9", strings.NewReader(url.Values{"title": {"New title\nignored"}}.Encode()))
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
		`hx-sync="#messages:queue last"`,
		`hx-disabled-elt="#message-form button[type='submit']"`,
		`hx-ext="sse"`,
		`sse-connect="/messages/tools?request=`,
		`sse-close="close"`,
		`sse-swap="tools"`,
		`hx-swap-oob="outerHTML"`,
		`name="request" value="`,
		`name="tool" value="webfetch"`,
		`class="completion-network-error" role="alert" hidden`,
		"Failed to complete message. Check your connection and retry.",
		`hx-post="/messages/retry?chat=1"`,
		`data-completion-error-message`,
	)
	requireNotContains(t, response.Body.String(), "every 200ms", `type="hidden" name="message"`)
	if count := strings.Count(response.Body.String(), `name="model" value="selected-model"`); count != 1 {
		t.Errorf("model input count = %d, want 1", count)
	}
	if count := strings.Count(response.Body.String(), `name="tool" value="webfetch"`); count != 1 {
		t.Errorf("tool input count = %d, want 1", count)
	}
}

func TestHTMXErrorsReturnTargetAppropriateHTML(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		database := openTestDatabase(t)
		response := httptest.NewRecorder()
		homeHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/?chat=invalid", nil))

		requireHTMLErrorResponse(t, response, http.StatusBadRequest, `<main hx-history="false">`, `id="messages"`, "A valid chat is required.")
	})

	t.Run("settings validation", func(t *testing.T) {
		database := openTestDatabase(t)
		t.Setenv("LLM_MODEL", "test-model")
		t.Setenv("LLM_ENDPOINT", "")
		request := httptest.NewRequest(http.MethodPost, "/settings?chat=1", strings.NewReader("%"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		settingsHandler(database, newTestToolRegistry(t))(response, request)

		requireHTMLErrorResponse(t, response, http.StatusBadRequest, `id="settings-page"`, `class="settings-form"`, "Invalid settings form.")
	})

	t.Run("settings storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		t.Setenv("LLM_MODEL", "test-model")
		t.Setenv("LLM_ENDPOINT", "")
		response := httptest.NewRecorder()
		settingsHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/settings?chat=1", nil))

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `id="settings-page"`, `class="settings-form"`, "Failed to load settings.")
	})

	t.Run("history load", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		response := httptest.NewRecorder()
		historyHandler(database)(response, httptest.NewRequest(http.MethodGet, "/history?chat=1", nil))

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `<li class="history-error"`, "Failed to load chat history.")
	})

	t.Run("rename validation", func(t *testing.T) {
		database := openTestDatabase(t)
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
		request := httptest.NewRequest(http.MethodPut, "/chats/1?current=1", strings.NewReader("title="))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		requireHistoryMutationError(t, response, http.StatusBadRequest, "A title is required.")
	})

	t.Run("rename storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /chats/{chat}", renameChatHandler(database))
		request := httptest.NewRequest(http.MethodPut, "/chats/1?current=1", strings.NewReader("title=New"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		requireHistoryMutationError(t, response, http.StatusInternalServerError, "Failed to rename chat.")
	})

	t.Run("delete storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /chats/{chat}", deleteChatHandler(database))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/chats/1?current=1", nil))

		requireHistoryMutationError(t, response, http.StatusInternalServerError, "Failed to delete chat.")
	})

	t.Run("message storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		response := postForm(t, messageHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages?chat=1", url.Values{
			"message": {"Hello"},
			"model":   {"test-model"},
		})

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `<article class="message" role="alert">`, "Failed to store message.")
	})

	t.Run("completion storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		toolCalls := newToolCallStore()
		response := postForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), "/messages/complete?chat=1", url.Values{
			"model":   {"test-model"},
			"request": {newToolCallRequest(t, toolCalls, 1)},
		})

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `class="message completion-error"`, "Failed to load conversation.")
	})

	t.Run("retry storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		response := postForm(t, messageRetryHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages/retry?chat=1", url.Values{
			"model": {"test-model"},
		})

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `class="message completion-error"`, "Failed to prepare retry.")
	})
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

func TestToolCallStoreUnclaimedTrackerExpiresWithoutAnotherCreate(t *testing.T) {
	toolCalls := newToolCallStore()
	toolCalls.unclaimedTTL = time.Millisecond
	requestID := newToolCallRequest(t, toolCalls, 1)
	tracker, ok := toolCalls.get(requestID)
	if !ok {
		t.Fatal("get unclaimed tracker failed")
	}
	waitForTestSignal(t, tracker.done, "unclaimed tracker expiry")
	if _, ok := toolCalls.get(requestID); ok {
		t.Error("expired unclaimed tracker remains stored")
	}
	replacementID, err := toolCalls.create(1)
	if err != nil {
		t.Fatalf("reuse chat after unclaimed expiry: %v", err)
	}
	toolCalls.delete(replacementID)
}

func TestMessageToolStreamHandlerStopsWhenUnclaimedTrackerExpires(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	response := newFlushingResponseRecorder()
	streamDone := make(chan struct{})
	go func() {
		messageToolStreamHandler(toolCalls)(response, httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil))
		close(streamDone)
	}()
	waitForTestSignal(t, response.flushes, "initial tool-call event")

	toolCalls.mu.Lock()
	entry := toolCalls.trackers[requestID]
	if entry != nil && entry.expiry != nil {
		entry.expiry.Reset(time.Millisecond)
	}
	toolCalls.mu.Unlock()
	if entry == nil || entry.expiry == nil {
		t.Fatal("unclaimed tracker has no expiry timer")
	}

	waitForTestSignal(t, streamDone, "unclaimed tracker stream expiry")
	if _, ok := toolCalls.get(requestID); ok {
		t.Error("expired stream tracker remains stored")
	}
	requireContains(t, response.Body.String(), "event: close\n")
}

func TestToolCallStoreClaimedTrackerStaysActivePastExpiry(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	tracker, ok := toolCalls.claim(requestID, 1)
	if !ok {
		t.Fatal("claim tracker failed")
	}

	toolCalls.expireUnclaimed(requestID)
	if _, err := toolCalls.create(1); !errors.Is(err, errChatCompletionActive) {
		t.Fatalf("create during started completion error = %v, want %v", err, errChatCompletionActive)
	}
	if current, ok := toolCalls.get(requestID); !ok || current != tracker {
		t.Error("started tracker was removed by expiry")
	}
	select {
	case <-tracker.done:
		t.Fatal("started tracker was closed by expiry")
	default:
	}

	toolCalls.delete(requestID)
	waitForTestSignal(t, tracker.done, "started tracker completion")
	replacementID, err := toolCalls.create(1)
	if err != nil {
		t.Fatalf("reuse chat after completion: %v", err)
	}
	toolCalls.delete(replacementID)
}

func TestMessageCompletionHandlerDoesNotReleaseActiveChatOnExpiry(t *testing.T) {
	database := openTestDatabase(t)
	insertAcceptedUser(t, database, 1, "Original question")
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-releaseRequest:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Original answer."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	registry := newTestToolRegistry(t)
	requestID := newToolCallRequest(t, toolCalls, 1)
	completionResponse := httptest.NewRecorder()
	completionDone := make(chan struct{})
	go func() {
		defer close(completionDone)
		form := url.Values{"model": {"selected-model"}, "request": {requestID}}
		request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		messageCompletionHandler(database, registry, toolCalls, nil)(completionResponse, request)
	}()
	waitForTestSignal(t, requestStarted, "active completion request")

	toolCalls.expireUnclaimed(requestID)
	replacement := postForm(t, messageHandler(database, registry, toolCalls), "/messages?chat=1", url.Values{
		"message": {"Replacement question"},
		"model":   {"selected-model"},
	})
	if replacement.Code != http.StatusConflict {
		t.Fatalf("replacement status = %d, want %d; body = %q", replacement.Code, http.StatusConflict, replacement.Body.String())
	}

	close(releaseRequest)
	waitForTestSignal(t, completionDone, "original completion")
	if completionResponse.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", completionResponse.Code, http.StatusOK, completionResponse.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get completed messages: %v", err)
	}
	if len(messages) != 2 || messages[0].Content != "Original question" || messages[1].Content != "Original answer." {
		t.Errorf("stored messages = %#v, want original question and answer only", messages)
	}
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
	tracker.observe(call, true, "")

	request := httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil)
	response := newFlushingResponseRecorder()
	streamDone := make(chan struct{})
	go func() {
		messageToolStreamHandler(toolCalls)(response, request)
		close(streamDone)
	}()
	waitForTestSignal(t, response.flushes, "running tool-call event")

	tracker.observe(call, false, "Tool error: upstream returned HTTP 429")
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
	requireContains(t, completedEvent,
		`class="tool-call tool-error"`,
		`class="tool-call-complete"`,
		`class="tool-call-error"`,
		"Failed tool:",
		"upstream returned HTTP 429",
	)
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

func TestMessageCompletionHandlerRejectsStaleGeneratedResponse(t *testing.T) {
	database := openTestDatabase(t)
	modelStarted := make(chan struct{})
	releaseModel := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(modelStarted)
		<-releaseModel
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Stale answer."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	insertAcceptedUser(t, database, 1, "First question")
	toolCalls := newToolCallStore()
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	registry := newTestToolRegistry(t)
	done := make(chan struct{})
	go func() {
		messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
		close(done)
	}()

	waitForTestSignal(t, modelStarted, "model request")
	insertAcceptedUser(t, database, 1, "Concurrent question")
	close(releaseModel)
	waitForTestSignal(t, done, "stale completion rejection")

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusConflict, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		"Conversation changed while the response was being generated. Retry the completion.",
		`hx-post="/messages/retry?chat=1"`,
	)
	var assistants int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&assistants); err != nil {
		t.Fatalf("count assistant messages: %v", err)
	}
	if assistants != 0 {
		t.Errorf("stored stale assistant count = %d, want 0", assistants)
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
	requireContains(t, response.Body.String(), "llm: reached maximum of 16 consecutive tool-call rounds")
	requireNotContains(t, response.Body.String(), "Failed to complete message.")
	if requestNumber != 17 {
		t.Errorf("model request count = %d, want 17", requestNumber)
	}
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
			err:  &llm.MaxToolRoundsError{Limit: 16},
			want: "llm: reached maximum of 16 consecutive tool-call rounds",
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
	fetchURL := "https://example.com/fetched"
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
			arguments, _ := json.Marshal(map[string]string{"url": fetchURL})
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
	registry, err := tools.NewRegistry(
		&tools.WebFetchTool{HTTPClient: &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			forwarded := request.Clone(request.Context())
			target, _ := url.Parse(fetched.URL)
			forwarded.URL.Scheme = target.Scheme
			forwarded.URL.Host = target.Host
			forwarded.Host = target.Host
			return http.DefaultTransport.RoundTrip(forwarded)
		})}},
	)
	if err != nil {
		t.Fatalf("create test tool registry: %v", err)
	}

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
	requireContains(t, body, "webfetch", fetchURL, "Final answer.", `class="tool-call-complete"`)
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
	requireContains(t, reloaded, "webfetch", fetchURL, "Final answer.", `class="tool-call-complete"`)
	requireNotContains(t, reloaded, "fetched content")
	if strings.Index(reloaded, "webfetch") > strings.Index(reloaded, "Final answer.") {
		t.Errorf("reloaded tool call does not precede answer: %s", reloaded)
	}
}

func TestMessageCompletionHandlerMarksFailedToolCalls(t *testing.T) {
	database := openTestDatabase(t)
	searxng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"current events","results":[]}`))
	}))
	defer searxng.Close()

	requestNumber := 0
	followupStarted := make(chan struct{}, 1)
	releaseFollowup := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		w.Header().Set("Content-Type", "application/json")
		if requestNumber == 1 {
			_, _ = w.Write([]byte(`{
				"model":"response-model",
				"choices":[{"message":{"role":"assistant","tool_calls":[{
					"id":"call-1",
					"type":"function",
					"function":{"name":"websearch","arguments":"{\"query\":\"current events\"}"}
				}]},"finish_reason":"tool_calls"}]
			}`))
			return
		}
		followupStarted <- struct{}{}
		select {
		case <-releaseFollowup:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Search unavailable."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	registry, err := tools.NewRegistry(tools.NewWebSearchTool(searxng.URL))
	if err != nil {
		t.Fatalf("create test tool registry: %v", err)
	}
	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Search the web")
	requestID := newToolCallRequest(t, toolCalls, 1)
	form := url.Values{
		"model":   {"selected-model"},
		"request": {requestID},
		"tool":    {"websearch"},
	}
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	completionDone := make(chan struct{})
	go func() {
		messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
		close(completionDone)
	}()
	select {
	case <-followupStarted:
	case <-time.After(2 * time.Second):
		close(releaseFollowup)
		t.Fatal("model follow-up request did not start")
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
	waitForTestSignal(t, statusResponse.flushes, "failed tool-call stream event")
	cancelStatus()
	waitForTestSignal(t, statusDone, "failed tool-call stream cancellation")
	requireContains(t, statusResponse.Body.String(),
		`class="tool-call tool-error"`,
		`class="tool-call-error"`,
		"websearch: SearXNG returned no results",
	)
	requireNotContains(t, statusResponse.Body.String(), `class="braille-spinner"`)

	close(releaseFollowup)
	waitForTestSignal(t, completionDone, "failed tool-call completion")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	requireContains(t, body,
		`class="tool-call tool-error"`,
		`class="tool-call-error"`,
		"current events",
		"websearch: SearXNG returned no results",
		"Search unavailable.",
	)

	t.Setenv("LLM_ENDPOINT", "")
	reloadResponse := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t))(reloadResponse, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("reload status = %d, want %d; body = %q", reloadResponse.Code, http.StatusOK, reloadResponse.Body.String())
	}
	requireContains(t, reloadResponse.Body.String(),
		`class="tool-call tool-error"`,
		`class="tool-call-error"`,
		"websearch: SearXNG returned no results",
	)
}

func TestResponsesProviderMetadataPersistsAcrossDatabaseReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := database.Exec(schema); err != nil {
		database.Close()
		t.Fatalf("initialize database: %v", err)
	}

	requests := make(chan []json.RawMessage, 3)
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request.Input
		requestNumber++
		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			_, _ = w.Write([]byte(`{
				"id":"response-1",
				"model":"response-model",
				"status":"completed",
				"output":[
					{"type":"reasoning","id":"reasoning-1","encrypted_content":"opaque-state","summary":[]},
					{"type":"function_call","id":"function-item-1","call_id":"call-1","name":"lookup","arguments":"{\"key\":\"value\"}"}
				]
			}`))
		case 2:
			_, _ = w.Write([]byte(`{
				"id":"response-2",
				"model":"response-model",
				"status":"completed",
				"output":[{"type":"message","id":"message-1","role":"assistant","content":[{"type":"output_text","text":"First answer."}]}]
			}`))
		case 3:
			_, _ = w.Write([]byte(`{
				"id":"response-3",
				"model":"response-model",
				"status":"completed",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second answer."}]}]
			}`))
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/responses")

	registry, err := tools.NewRegistry(responsePersistenceTestTool{})
	if err != nil {
		database.Close()
		t.Fatalf("create tool registry: %v", err)
	}
	insertAcceptedUser(t, database, 1, "First question")
	toolCalls := newToolCallStore()
	firstResponse := postForm(t, messageCompletionHandler(database, registry, toolCalls, nil), "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
		"tool":    {"lookup"},
	})
	if firstResponse.Code != http.StatusOK {
		database.Close()
		t.Fatalf("first completion status = %d, want %d; body = %q", firstResponse.Code, http.StatusOK, firstResponse.Body.String())
	}
	<-requests
	<-requests
	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer database.Close()
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate reopened database: %v", err)
	}
	storedMessages, err := kritui_db.GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("load stored Responses messages: %v", err)
	}
	if len(storedMessages) != 4 {
		t.Fatalf("stored message count = %d, want 4", len(storedMessages))
	}
	storedOutput := storedMessages[1].ProviderMetadata.ResponsesOutput()
	if len(storedOutput) != 2 {
		t.Fatalf("stored provider output count = %d, want 2", len(storedOutput))
	}
	if err := persistUserMessage(context.Background(), database, 1, "Second question", []string{"lookup"}, nil); err != nil {
		t.Fatalf("store second user message: %v", err)
	}

	toolCalls = newToolCallStore()
	secondResponse := postForm(t, messageCompletionHandler(database, registry, toolCalls, nil), "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
		"tool":    {"lookup"},
	})
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second completion status = %d, want %d; body = %q", secondResponse.Code, http.StatusOK, secondResponse.Body.String())
	}
	continuedInput := <-requests
	if len(continuedInput) != 7 {
		t.Fatalf("continued input length = %d, want 7", len(continuedInput))
	}
	reasoning := decodeResponseInputItem(t, continuedInput[2])
	if reasoning["type"] != "reasoning" || reasoning["id"] != "reasoning-1" || reasoning["encrypted_content"] != "opaque-state" {
		t.Errorf("continued reasoning item = %#v, want original opaque item", reasoning)
	}
	functionCall := decodeResponseInputItem(t, continuedInput[3])
	if functionCall["type"] != "function_call" || functionCall["id"] != "function-item-1" || functionCall["call_id"] != "call-1" {
		t.Errorf("continued function-call item = %#v, want original provider item", functionCall)
	}
	functionOutput := decodeResponseInputItem(t, continuedInput[4])
	if functionOutput["type"] != "function_call_output" || functionOutput["call_id"] != "call-1" || functionOutput["output"] != "found value" {
		t.Errorf("continued function output = %#v", functionOutput)
	}
	finalMessage := decodeResponseInputItem(t, continuedInput[5])
	if finalMessage["type"] != "message" || finalMessage["id"] != "message-1" {
		t.Errorf("continued final message = %#v, want original provider message item", finalMessage)
	}
	secondUser := decodeResponseInputItem(t, continuedInput[6])
	if secondUser["role"] != "user" || secondUser["content"] != "Second question" {
		t.Errorf("continued user input = %#v, want second question", secondUser)
	}
}

func TestMigrateDatabaseHistoricalSchemas(t *testing.T) {
	for _, legacyVersion := range []int{1, 2, 3, 4, 5, 6} {
		t.Run("legacy-version-"+strconv.Itoa(legacyVersion), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data.db")
			database := openLegacyTestDatabase(t, path, legacyVersion)

			if err := migrateDatabase(database); err != nil {
				t.Fatalf("migrate database: %v", err)
			}
			assertMigratedDatabase(t, database)
			if err := migrateDatabase(database); err != nil {
				t.Fatalf("run migrations twice: %v", err)
			}
			if err := database.Close(); err != nil {
				t.Fatalf("close migrated database: %v", err)
			}

			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatalf("reopen migrated database: %v", err)
			}
			t.Cleanup(func() { database.Close() })
			if err := migrateDatabase(database); err != nil {
				t.Fatalf("migrate reopened database: %v", err)
			}
			assertMigratedDatabase(t, database)
		})
	}
}

func TestMigrateDatabaseRejectsMalformedLegacySchema(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE chats (id INTEGER PRIMARY KEY) STRICT`); err != nil {
		t.Fatalf("create malformed schema: %v", err)
	}

	err = migrateDatabase(database)
	if err == nil || !strings.Contains(err.Error(), "required table messages does not exist") {
		t.Fatalf("migrate malformed database error = %v, want missing messages table", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 0 {
		t.Errorf("schema version = %d, want 0 after rollback", version)
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

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func postForm(t *testing.T, handler http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func serveRawForm(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func paddedFormBody(t *testing.T, base string, size int64) string {
	t.Helper()
	const separator = "&padding="
	padding := int(size) - len(base) - len(separator)
	if padding < 0 {
		t.Fatalf("form base length %d exceeds requested body size %d", len(base), size)
	}
	return base + separator + strings.Repeat("x", padding)
}

func multipartBodyOfSize(t *testing.T, size int64) ([]byte, string) {
	t.Helper()
	const boundary = "kritui-test-boundary"
	build := func(padding int) ([]byte, string) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.SetBoundary(boundary); err != nil {
			t.Fatalf("set multipart boundary: %v", err)
		}
		if err := writer.WriteField("message", "hello"); err != nil {
			t.Fatalf("write multipart message: %v", err)
		}
		if err := writer.WriteField("model", "test-model"); err != nil {
			t.Fatalf("write multipart model: %v", err)
		}
		if err := writer.WriteField("padding", strings.Repeat("x", padding)); err != nil {
			t.Fatalf("write multipart padding: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close multipart form: %v", err)
		}
		return body.Bytes(), writer.FormDataContentType()
	}

	base, _ := build(0)
	padding := int(size) - len(base)
	if padding < 0 {
		t.Fatalf("multipart overhead %d exceeds requested body size %d", len(base), size)
	}
	body, contentType := build(padding)
	if int64(len(body)) != size {
		t.Fatalf("multipart body size = %d, want %d", len(body), size)
	}
	return body, contentType
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

func requireHTMLErrorResponse(t *testing.T, response *httptest.ResponseRecorder, status int, values ...string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, status, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want HTML", contentType)
	}
	requireContains(t, response.Body.String(), `role="alert"`)
	requireContains(t, response.Body.String(), values...)
}

func requireHistoryMutationError(t *testing.T, response *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	requireHTMLErrorResponse(t, response, status, `id="history-error"`, message)
	if target := response.Header().Get("HX-Retarget"); target != "#history-error" {
		t.Errorf("HX-Retarget = %q, want #history-error", target)
	}
	if swap := response.Header().Get("HX-Reswap"); swap != "outerHTML" {
		t.Errorf("HX-Reswap = %q, want outerHTML", swap)
	}
	requireNotContains(t, response.Body.String(), `id="history-entries"`)
}

func decodeResponseInputItem(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode Responses input item: %v", err)
	}
	return item
}

type responsePersistenceTestTool struct{}

func (responsePersistenceTestTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "lookup",
		Description: "Looks up a test value",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`),
	}
}

func (responsePersistenceTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "found value", nil
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

func openLegacyTestDatabase(t *testing.T, path string, version int) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}

	chatTools := ""
	if version >= 3 {
		chatTools = `tools TEXT NOT NULL DEFAULT '[]',`
	}
	messageModel := ""
	if version >= 2 {
		messageModel = `model TEXT,`
	}
	messageUsage := ""
	if version >= 4 {
		messageUsage = `total_tokens INTEGER, cost REAL,`
	}
	providerMetadata := ""
	if version >= 6 {
		providerMetadata = `provider_metadata TEXT,`
	}
	settings := ""
	if version >= 5 {
		settings = `CREATE TABLE settings (name TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;`
	}
	fixture := `
		PRAGMA foreign_keys = ON;
		CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			` + chatTools + `
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT;
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			role TEXT NOT NULL CHECK (role IN ('system', 'developer', 'user', 'assistant', 'tool')),
			content TEXT NOT NULL DEFAULT '',
			` + messageModel + messageUsage + providerMetadata + `
			tool_calls TEXT,
			tool_call_id TEXT,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			UNIQUE (chat_id, position)
		) STRICT;
		` + settings + `
		INSERT INTO chats (id, title) VALUES (7, 'legacy chat');
		INSERT INTO messages (chat_id, position, role, content) VALUES (7, 0, 'user', 'legacy message');
	`
	if _, err := database.Exec(fixture); err != nil {
		database.Close()
		t.Fatalf("initialize legacy version %d: %v", version, err)
	}
	return database
}

func assertMigratedDatabase(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx := context.Background()

	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Errorf("schema version = %d, want %d", version, len(databaseMigrations))
	}

	chats, err := kritui_db.GetChats(ctx, database)
	if err != nil {
		t.Fatalf("get migrated chats: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != 7 || chats[0].Title != "legacy chat" {
		t.Fatalf("migrated chats = %#v, want preserved chat 7", chats)
	}
	tools, err := kritui_db.GetChatTools(ctx, database, 7)
	if err != nil {
		t.Fatalf("get migrated chat tools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("migrated chat tools = %#v, want empty", tools)
	}
	messages, err := kritui_db.GetMessages(ctx, database, 7)
	if err != nil {
		t.Fatalf("get migrated messages: %v", err)
	}
	if len(messages) == 0 || messages[0].Content != "legacy message" {
		t.Fatalf("migrated messages = %#v, want preserved legacy message", messages)
	}

	if len(messages) == 1 {
		tokens := 42
		cost := 0.25
		if _, err := kritui_db.InsertMessage(ctx, database, 7, 1, llm.Message{
			Role:        "assistant",
			Content:     "migrated response",
			Model:       "migrated-model",
			TotalTokens: &tokens,
			Cost:        &cost,
		}); err != nil {
			t.Fatalf("append migrated message: %v", err)
		}
	}
	if err := kritui_db.SetDefaultModel(ctx, database, "migrated-model"); err != nil {
		t.Fatalf("set migrated setting: %v", err)
	}
	model, err := kritui_db.GetDefaultModel(ctx, database, "fallback")
	if err != nil {
		t.Fatalf("get migrated setting: %v", err)
	}
	if model != "migrated-model" {
		t.Errorf("migrated setting = %q, want migrated-model", model)
	}
}
