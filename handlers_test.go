package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"seesharpsi/kritui/commands"
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

func TestLogoHandlerOffersPNGDownload(t *testing.T) {
	response := httptest.NewRecorder()
	logoHandler(staticFiles)(response, httptest.NewRequest(http.MethodGet, "/logo.png", nil))

	if status := response.Code; status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", contentType)
	}
	if contentDisposition := response.Header().Get("Content-Disposition"); contentDisposition != `attachment; filename="kritui-logo.png"` {
		t.Errorf("Content-Disposition = %q, want download filename", contentDisposition)
	}
	body := response.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("body is empty")
	}
	if len(body) < 8 {
		t.Fatalf("body is shorter than PNG signature: %d bytes", len(body))
	}
	if !bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Errorf("body does not begin with PNG signature: %x", body[:min(len(body), 8)])
	}
}

func TestStaticHandlerServesVersionedAssetsWithImmutableCache(t *testing.T) {
	response := httptest.NewRecorder()
	staticHandler(staticFiles).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/htmx.min.js?v=6", nil))

	if status := response.Code; status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != staticAssetCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cacheControl, staticAssetCacheControl)
	}
	if body := response.Body.String(); !strings.Contains(body, "registerExtension") {
		t.Errorf("body does not contain registerExtension")
	}
}

func TestStaticStylesDefineGlobalRequestOverlay(t *testing.T) {
	styles, err := staticFiles.ReadFile("static/styles.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	start := strings.Index(string(styles), ".request-overlay {")
	visible := strings.Index(string(styles), ".request-overlay.htmx-indicator.htmx-request {")
	if start == -1 || visible == -1 || visible < start {
		t.Fatalf("request-overlay styles missing or out of order: start = %d, visible = %d", start, visible)
	}
	requireContains(t, string(styles)[start:visible],
		"position: fixed;",
		"inset: 0;",
		"z-index: 10000;",
		"background: transparent;",
		"opacity: 0;",
		"visibility: hidden;",
		"pointer-events: none;",
		".braille-spinner::before {",
		"font-size: 4rem;",
	)
	requireContains(t, string(styles)[visible:],
		"opacity: 1;",
		"visibility: visible;",
		"pointer-events: none;",
		"transition: none;",
	)
	requireNotContains(t, string(styles)[start:visible], "transition:")
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
			_, err := persistUserMessage(context.Background(), database, chatID, "message", nil, []string{}, nil)
			errors <- err
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

func TestHomeHandlerAllocatesNextChatIDAtIndex(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (2, ''), (5, '')`); err != nil {
		t.Fatalf("insert chats: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?view=compact", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("unexpected Location header %q", location)
	}
	requireContains(t, response.Body.String(), `hx-post="/messages?chat=6"`, `hx-replace-url="/?chat=6&amp;view=compact"`)

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&count); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if count != 3 {
		t.Errorf("chat count = %d, want 3", count)
	}
}

func TestHomeHandlerRendersFirstChatIDAtIndexForEmptyDatabase(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(), `hx-post="/messages?chat=1"`, `hx-replace-url="/?chat=1"`)
}

func TestHomeHandlerAllocatesConcurrentChatsWithIsolatedMessages(t *testing.T) {
	database := openTestDatabase(t)
	registry := newTestToolRegistry(t)
	commandRegistry := newTestCommandRegistry(t, database)
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			homeHandler(database, registry, commandRegistry, newToolCallStore())(responses[index], httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	close(start)
	wait.Wait()

	chatIDs := make([]int64, len(responses))
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response %d status = %d, want %d", index, response.Code, http.StatusOK)
		}
		chatIDs[index] = messageFormChatID(t, response.Body.String())
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
		response := postForm(t, messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls), "/messages?chat="+strconv.FormatInt(chatID, 10), forms[index])
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
	commandRegistry := newTestCommandRegistry(t, database)
	first := httptest.NewRecorder()
	homeHandler(database, registry, commandRegistry, newToolCallStore())(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if _, err := database.Exec(`UPDATE chats SET created_at = '2000-01-01T00:00:00.000Z'`); err != nil {
		t.Fatalf("age abandoned chat: %v", err)
	}

	history := httptest.NewRecorder()
	historyHandler(database)(history, httptest.NewRequest(http.MethodGet, "/history?chat=1", nil))
	requireContains(t, history.Body.String(), "No saved chats yet.")

	second := httptest.NewRecorder()
	homeHandler(database, registry, commandRegistry, newToolCallStore())(second, httptest.NewRequest(http.MethodGet, "/", nil))
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&count); err != nil {
		t.Fatalf("count allocated chats: %v", err)
	}
	if count != 1 {
		t.Errorf("allocated chat count = %d, want one current allocation", count)
	}
	requireContains(t, second.Body.String(), `hx-post="/messages?chat=2"`, `hx-replace-url="/?chat=2"`)
}

func TestHomeHandlerRendersStoredMessages(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO messages (chat_id, position, role, content, model, tool_call_id) VALUES
			(8, 0, 'user', 'Earlier question', NULL, NULL),
			(8, 1, 'assistant', '', 'stored-model', NULL),
			(8, 2, 'tool', 'result', NULL, 'call-1'),
			(8, 3, 'assistant', 'Earlier answer', 'stored-model', NULL);
		INSERT INTO message_tool_calls
			(message_id, message_role, position, call_id, call_type, function_name, arguments)
		SELECT id, 'assistant', 0, 'call-1', 'function', 'webfetch', '{"url":"https://example.com"}'
		FROM messages WHERE chat_id = 8 AND position = 1;
	`); err != nil {
		t.Fatalf("insert chat history: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("unexpected Location header %q", location)
	}
	requireContains(t, response.Body.String(),
		"Earlier question",
		"Earlier answer",
		`class="message-copy-button"`,
		`aria-label="Copy message"`,
		`class="message-edit-toggle"`,
		`hx-put="/chats/8/messages/1"`,
		`hx-include="[form='message-form'][name='model']:checked, [form='message-form'][name='tool']:checked, [form='message-form'][name='append']:checked"`,
		`/static/htmx.min.js?v=13`,
		`/static/hx-sse.js?v=13`,
		`/static/app.js?v=13`,
		`/static/styles.css?v=13`,
		`<body hx-indicator:inherited="global #request-overlay">`,
		`<div id="request-overlay" class="request-overlay htmx-indicator" role="status" aria-live="polite" aria-label="Loading">`,
		`<span class="braille-spinner" aria-hidden="true"></span>`,
		`defaultTimeout&#34;:0`,
		`defaultSettleDelay&#34;:20`,
		`extensions&#34;:&#34;sse`,
		`hx-sync="#messages:drop"`,
		"<strong>stored-model</strong>",
		`class="tool-call-toggle"`,
		`aria-expanded="false"`,
	)
	if count := strings.Count(response.Body.String(), `class="message-edit-toggle"`); count != 1 {
		t.Errorf("message edit button count = %d, want 1", count)
	}
	if count := strings.Count(response.Body.String(), `class="message-copy-button"`); count != 1 {
		t.Errorf("message copy button count = %d, want 1", count)
	}
	requireNotContains(t, response.Body.String(), "What would you like to discuss?", "begin a convo...", "<strong>assistant</strong>", `role="button"`, `hx-replace-url`)
	requireNotContains(t, response.Body.String(), `hx-history="false"`)
}

func TestHomeHandlerRendersEmptyChatPrompt(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	requireContains(t, response.Body.String(), "begin a convo...")
	requireNotContains(t, response.Body.String(), "What would you like to discuss?")
}

func TestHomeHandlerRendersCommandAutocomplete(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	requireContains(t, body,
		`id="message"`,
		`role="combobox"`,
		`aria-autocomplete="list"`,
		`aria-controls="command-autocomplete"`,
		`aria-expanded="false"`,
		`id="command-autocomplete"`,
		`role="listbox"`,
		`aria-label="Slash commands"`,
		`id="command-option-new"`,
		`data-command-name="new"`,
		`>Start a new chat</span>`,
		`id="command-option-undo"`,
		`>Undo the last message</span>`,
		`id="command-option-redo"`,
		`>Redo the last undone message</span>`,
		`id="command-option-history"`,
		`>Open chat history</span>`,
		`id="command-option-settings"`,
		`>Open settings</span>`,
		`id="command-option-rename"`,
		`data-command-requires-arguments="true"`,
		`>Rename the current chat</span>`,
	)
	if count := strings.Count(body, `data-command-requires-arguments="true"`); count != 1 {
		t.Errorf("required-argument command count = %d, want 1", count)
	}
	previous := -1
	for _, name := range []string{"new", "undo", "redo", "history", "settings", "rename"} {
		index := strings.Index(body, `id="command-option-`+name+`"`)
		if index <= previous {
			t.Fatalf("command %q index = %d after %d; want registry order", name, index, previous)
		}
		previous = index
	}
}

func TestHomeHandlerPreloadsSettingsAndEmptyHistoryShell(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (7, 'Loaded lazily')`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

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
		`hx-include="#append-picker input[name='append']:checked, #tool-picker input[name='tool']:checked"`,
		`name="append_selection" value="1"`,
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
	requireContains(t, string(script), `matches('[data-panel-initial-focus]')`)
	requireContains(t, string(script), `panelButton?.focus()`)
	requireContains(t, string(script), `document.addEventListener('kritui:command'`)
	requireContains(t, string(script), `messageInput.value = ''`)
	requireContains(t, string(script), `document.addEventListener('input'`)
	requireContains(t, string(script), `document.addEventListener('keydown'`)
	requireContains(t, string(script), `moveCommandSelection(input, 1)`)
	requireContains(t, string(script), `activateCommandOption(input, selected)`)
	requireContains(t, string(script), `option.dataset.commandRequiresArguments === 'true'`)
	requireContains(t, string(script), `input.form?.requestSubmit()`)
	requireContains(t, string(script), `if (!event.detail?.preserveInput)`)
	requireContains(t, string(script), `input[name^="mcp_authorization_"]`)
	requireNotContains(t, string(script), `querySelector('.history-loader')`, "htmx.trigger", "htmx:oobBeforeSwap", "htmx:oobAfterSwap", "htmx:configRequest")
	requireContains(t, string(script), `settingsPage.scrollTop = settingsPageScrollTop`)
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
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

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
	seedDefaultModel(t, database, "stored-default")

	request := httptest.NewRequest(http.MethodGet, "/?chat=1", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

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
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

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
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO chat_tools (chat_id, position, name) VALUES (8, 0, 'websearch');
	`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")

	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

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

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, httptest.NewRequest(http.MethodGet, "/?chat=8", nil))

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
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls), "/messages?chat=3", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	names, err := kritui_db.GetChatTools(context.Background(), database, 3)
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
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

func TestMessageHandlerPersistsUploadedImages(t *testing.T) {
	database := openTestDatabase(t)
	toolCalls := newToolCallStore()
	pngBytes := testImageBytes(t, "png", 2, 3)
	jpegBytes := testImageBytes(t, "jpeg", 4, 5)
	body, contentType := multipartImageRequest(t, "", []testUpload{{`..\\nested\\` + strings.Repeat("x", 260) + ".png", "text/plain", pngBytes}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=41", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls)(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %q", response.Code, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 41)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != "" || len(messages[0].Images) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	image := messages[0].Images[0]
	if image.MediaType != "image/png" || image.Width != 2 || image.Height != 3 || !bytes.Equal(image.Data, pngBytes) {
		t.Errorf("stored image = %#v", image)
	}
	if len([]rune(image.Filename)) != 255 {
		t.Errorf("filename = %q", image.Filename)
	}
	if response.Body.Len() == 0 {
		t.Error("pending response empty")
	}

	webpBytes := testWebPBytes(t)
	body, contentType = multipartImageRequest(t, "ordered", []testUpload{{"first.jpg", "image/jpeg", jpegBytes}, {"second.png", "image/png", pngBytes}, {"third.webp", "image/webp", webpBytes}})
	request = httptest.NewRequest(http.MethodPost, "/messages?chat=42", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response = httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ordered status = %d", response.Code)
	}
	messages, err = kritui_db.GetMessages(context.Background(), database, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Images) != 3 || messages[0].Images[0].Filename != "first.jpg" || messages[0].Images[1].Filename != "second.png" || messages[0].Images[2].MediaType != "image/webp" || !bytes.Equal(messages[0].Images[2].Data, webpBytes) {
		t.Fatalf("order = %#v", messages)
	}
}

func TestMessageHandlerRejectsInvalidImageUploads(t *testing.T) {
	tests := []struct {
		name    string
		message string
		files   []testUpload
		status  int
	}{
		{"corrupt", "", []testUpload{{"x.png", "image/png", []byte("bad")}}, 400},
		{"gif", "", []testUpload{{"x.gif", "image/gif", []byte("GIF89a")}}, 400},
		{"unknown field", "", []testUpload{{"x.png", "image/png", testImageBytes(t, "png", 1, 1)}}, 400},
		{"command", "/new", []testUpload{{"x.png", "image/png", testImageBytes(t, "png", 1, 1)}}, 400},
		{"dimensions", "", []testUpload{{"x.png", "image/png", testImageBytes(t, "png", 8001, 1)}}, 400},
		{"too many", "", []testUpload{{"1.png", "", testImageBytes(t, "png", 1, 1)}, {"2.png", "", testImageBytes(t, "png", 1, 1)}, {"3.png", "", testImageBytes(t, "png", 1, 1)}, {"4.png", "", testImageBytes(t, "png", 1, 1)}, {"5.png", "", testImageBytes(t, "png", 1, 1)}}, 413},
		{"too large", "", []testUpload{{"large.png", "", bytes.Repeat([]byte("x"), int(maxImageBytes)+1)}}, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDatabase(t)
			body, ct := multipartImageRequest(t, tt.message, tt.files)
			if tt.name == "unknown field" {
				body, ct = multipartImageRequestField(t, "other", tt.files)
			}
			req := httptest.NewRequest(http.MethodPost, "/messages?chat=51", bytes.NewReader(body))
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			messageHandler(db, newTestToolRegistry(t), newTestCommandRegistry(t, db), newToolCallStore())(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			messages, _ := kritui_db.GetMessages(context.Background(), db, 51)
			if len(messages) != 0 {
				t.Fatalf("persisted messages = %d", len(messages))
			}
		})
	}
}

func TestMessageHandlerModelImageCapability(t *testing.T) {
	for _, unsupported := range []bool{true, false} {
		t.Run(fmt.Sprint(unsupported), func(t *testing.T) {
			db := openTestDatabase(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if unsupported {
					_, _ = io.WriteString(w, `{"data":[{"id":"vision-model","capabilities":{"image_input":{"supported":false}},"architecture":{"input_modalities":["text"]}}]}`)
				} else {
					_, _ = io.WriteString(w, `{"data":[]}`)
				}
			}))
			defer server.Close()
			t.Setenv("LLM_KEY", "test-key")
			t.Setenv("LLM_ENDPOINT", server.URL+"/responses")
			body, ct := multipartImageRequest(t, "", []testUpload{{"x.png", "", testImageBytes(t, "png", 1, 1)}})
			req := httptest.NewRequest(http.MethodPost, "/messages?chat=61", bytes.NewReader(body))
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			messageHandler(db, newTestToolRegistry(t), newTestCommandRegistry(t, db), newToolCallStore())(rec, req)
			want := http.StatusOK
			if unsupported {
				want = http.StatusBadRequest
			}
			if rec.Code != want {
				t.Fatalf("status = %d, want %d", rec.Code, want)
			}
			messages, _ := kritui_db.GetMessages(context.Background(), db, 61)
			if (len(messages) != 0) == !unsupported {
				return
			}
			t.Fatalf("persistence mismatch: %d", len(messages))
		})
	}
}

func TestMessageHandlerExecutesNavigationCommandsWithoutPersistence(t *testing.T) {
	database := openTestDatabase(t)
	chatID := seedChat(t, database, "Existing", nil, nil)
	toolCalls := newToolCallStore()
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls)

	tests := []struct {
		name       string
		message    string
		wantHeader string
		wantValue  string
	}{
		{name: "new", message: "/new", wantHeader: "HX-Redirect", wantValue: "/"},
		{name: "history", message: "/history", wantHeader: "HX-Trigger", wantValue: `{"kritui:command":{"panel":"history-page"}}`},
		{name: "settings", message: "/settings", wantHeader: "HX-Trigger", wantValue: `{"kritui:command":{"panel":"settings-page"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postForm(t, handler, "/messages?chat="+strconv.FormatInt(chatID, 10), url.Values{
				"message": {test.message},
				"tool":    {"not-a-tool"},
			})
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
			}
			if got := response.Header().Get(test.wantHeader); got != test.wantValue {
				t.Errorf("%s = %q, want %q", test.wantHeader, got, test.wantValue)
			}
			if response.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", response.Body.String())
			}

			requestID, err := toolCalls.create(chatID, "", nil)
			if err != nil {
				t.Fatalf("command left completion tracker: %v", err)
			}
			toolCalls.delete(requestID)
		})
	}

	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 0 {
		t.Errorf("stored messages = %d, want 0", messages)
	}
}

func TestMessageHandlerRenamesCurrentChatCommand(t *testing.T) {
	database := openTestDatabase(t)
	chatID := seedChat(t, database, "Old title", nil, nil)
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())

	response := postForm(t, handler, "/messages?chat="+strconv.FormatInt(chatID, 10), url.Values{"message": {"/rename New title"}})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
	if got := response.Header().Get("HX-Trigger"); got != `{"kritui:command":{}}` {
		t.Errorf("HX-Trigger = %q", got)
	}
	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = ?`, chatID).Scan(&title); err != nil {
		t.Fatalf("get renamed chat: %v", err)
	}
	if title != "New title" {
		t.Errorf("title = %q, want New title", title)
	}
	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 0 {
		t.Errorf("stored messages = %d, want 0", messages)
	}
}

func TestMessageHandlerUndoesAndRedoesLatestTurn(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (1, 'Conversation');
		INSERT INTO messages (chat_id, position, role, content, model) VALUES
			(1, 0, 'user', 'Earlier question', NULL),
			(1, 1, 'assistant', 'Earlier answer', 'model-a'),
			(1, 2, 'user', 'Latest <question>', NULL),
			(1, 3, 'assistant', 'Latest answer', 'model-b');
	`); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())

	undo := postForm(t, handler, "/messages?chat=1", url.Values{"message": {"/undo"}})
	if undo.Code != http.StatusOK {
		t.Fatalf("undo status = %d, want %d; body = %q", undo.Code, http.StatusOK, undo.Body.String())
	}
	if got := undo.Header().Get("HX-Retarget"); got != "#message-list" {
		t.Errorf("undo HX-Retarget = %q", got)
	}
	if got := undo.Header().Get("HX-Reswap"); got != "outerHTML" {
		t.Errorf("undo HX-Reswap = %q", got)
	}
	if got := undo.Header().Get("HX-Trigger"); got != `{"kritui:command":{"preserveInput":true}}` {
		t.Errorf("undo HX-Trigger = %q", got)
	}
	requireContains(t, undo.Body.String(),
		`id="message-list"`,
		"Earlier question",
		"Earlier answer",
		`id="message"`,
		`>Latest &lt;question&gt;</textarea>`,
		`hx-swap-oob="outerHTML"`,
	)
	requireNotContains(t, undo.Body.String(), "Latest answer")
	active, err := kritui_db.GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get messages after undo: %v", err)
	}
	if len(active) != 2 || active[1].Content != "Earlier answer" {
		t.Errorf("active messages after undo = %#v", active)
	}
	var hidden int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE undo_sequence IS NOT NULL`).Scan(&hidden); err != nil {
		t.Fatalf("count hidden messages: %v", err)
	}
	if hidden != 2 {
		t.Errorf("hidden messages = %d, want 2", hidden)
	}

	redo := postForm(t, handler, "/messages?chat=1", url.Values{"message": {"/redo"}})
	if redo.Code != http.StatusOK {
		t.Fatalf("redo status = %d, want %d; body = %q", redo.Code, http.StatusOK, redo.Body.String())
	}
	requireContains(t, redo.Body.String(), "Latest &lt;question&gt;", "Latest answer", `id="message"`, `></textarea>`)
	active, err = kritui_db.GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get messages after redo: %v", err)
	}
	if len(active) != 4 || active[3].Content != "Latest answer" {
		t.Errorf("active messages after redo = %#v", active)
	}
}

func TestMessageHandlerNewMessageDiscardsRedoHistory(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "test-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (1, 'Conversation');
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'Original question'),
			(1, 1, 'assistant', 'Original answer');
	`); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := kritui_db.UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("undo original turn: %v", err)
	}
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())

	response := postForm(t, handler, "/messages?chat=1", url.Values{
		"message": {"Replacement question"},
		"model":   {"test-model"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("replacement status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get replacement messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Content != "Replacement question" {
		t.Errorf("replacement messages = %#v", messages)
	}
	var hidden int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE undo_sequence IS NOT NULL`).Scan(&hidden); err != nil {
		t.Fatalf("count hidden messages: %v", err)
	}
	if hidden != 0 {
		t.Errorf("hidden messages = %d, want 0", hidden)
	}

	redo := postForm(t, handler, "/messages?chat=1", url.Values{"message": {"/redo"}})
	if redo.Code != http.StatusConflict {
		t.Fatalf("redo status = %d, want %d; body = %q", redo.Code, http.StatusConflict, redo.Body.String())
	}
	requireContains(t, redo.Body.String(), "Nothing to redo.")
}

func TestMessageHandlerRejectsInvalidSlashCommands(t *testing.T) {
	database := openTestDatabase(t)
	chatID := seedChat(t, database, "Existing", nil, nil)
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())
	tests := []struct {
		name    string
		chatID  int64
		message string
		status  int
		want    string
	}{
		{name: "unknown", chatID: chatID, message: "/missing", status: http.StatusBadRequest, want: "Unknown command /missing."},
		{name: "malformed", chatID: chatID, message: "/History", status: http.StatusBadRequest, want: "Invalid slash command."},
		{name: "navigation arguments", chatID: chatID, message: "/history extra", status: http.StatusBadRequest, want: "/history does not accept arguments."},
		{name: "undo arguments", chatID: chatID, message: "/undo extra", status: http.StatusBadRequest, want: "/undo does not accept arguments."},
		{name: "rename title", chatID: chatID, message: "/rename", status: http.StatusBadRequest, want: "Usage: /rename &lt;title&gt;."},
		{name: "missing chat", chatID: chatID + 1, message: "/rename New title", status: http.StatusNotFound, want: "Chat not found."},
		{name: "nothing to undo", chatID: chatID, message: "/undo", status: http.StatusConflict, want: "Nothing to undo."},
		{name: "nothing to redo", chatID: chatID, message: "/redo", status: http.StatusConflict, want: "Nothing to redo."},
		{name: "undo missing chat", chatID: chatID + 1, message: "/undo", status: http.StatusNotFound, want: "Chat not found."},
		{name: "redo missing chat", chatID: chatID + 1, message: "/redo", status: http.StatusNotFound, want: "Chat not found."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postForm(t, handler, "/messages?chat="+strconv.FormatInt(test.chatID, 10), url.Values{"message": {test.message}})
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.status, response.Body.String())
			}
			requireContains(t, response.Body.String(), test.want)
		})
	}

	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 0 {
		t.Errorf("stored messages = %d, want 0", messages)
	}
}

func TestMessageHandlerRequiresSlashAsFirstCharacter(t *testing.T) {
	database := openTestDatabase(t)
	toolCalls := newToolCallStore()
	response := postForm(t,
		messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls),
		"/messages?chat=3",
		url.Values{"message": {" /new"}, "model": {"test-model"}},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want empty", got)
	}
	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE chat_id = 3`).Scan(&content); err != nil {
		t.Fatalf("get message: %v", err)
	}
	if content != "/new" {
		t.Errorf("stored content = %q, want /new", content)
	}
}

func TestMessageRetryHandlerIgnoresUndoneUserMessage(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'undone');
	`); err != nil {
		t.Fatalf("insert user message: %v", err)
	}
	if _, err := kritui_db.UndoLatestTurn(context.Background(), database, 1); err != nil {
		t.Fatalf("undo user message: %v", err)
	}

	response := postForm(t, messageRetryHandler(database, newTestToolRegistry(t), newToolCallStore()), "/messages/retry?chat=1", url.Values{
		"model": {"test-model"},
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusConflict, response.Body.String())
	}
	requireContains(t, response.Body.String(), "No message is waiting for completion.")
}

func TestMessageHandlerAppliesAndPersistsPromptAppends(t *testing.T) {
	database := openTestDatabase(t)
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore()), "/messages?chat=3", url.Values{
		"message": {"Hello"},
		"model":   {"selected-model"},
		"append":  {"research", "link-check"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	defaults := kritui_db.DefaultPromptAppends()

	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE chat_id = 3`).Scan(&content); err != nil {
		t.Fatalf("query message: %v", err)
	}
	if content != "Hello" {
		t.Errorf("stored message = %q, want original Hello", content)
	}
	wantPromptAppendTexts := []string{defaults[1].Text, defaults[0].Text}
	storedMessages, err := kritui_db.GetMessages(context.Background(), database, 3)
	if err != nil {
		t.Fatalf("get stored messages: %v", err)
	}
	if len(storedMessages) != 1 || !slices.Equal(storedMessages[0].PromptAppendTexts, wantPromptAppendTexts) {
		t.Errorf("loaded message prompt append texts = %#v, want %v", storedMessages, wantPromptAppendTexts)
	}
	selected, err := kritui_db.GetChatPromptAppendIDs(context.Background(), database, 3)
	if err != nil {
		t.Fatalf("get chat prompt append IDs: %v", err)
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
	messageIndex := strings.Index(body, `<article class="message user"`)
	if appendIndex == -1 || messageIndex == -1 || appendIndex <= messageIndex {
		t.Errorf("append details index = %d, user message index = %d; want details after message", appendIndex, messageIndex)
	}
}

func TestMessageHandlerRejectsUnknownPromptAppend(t *testing.T) {
	database := openTestDatabase(t)
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore()), "/messages?chat=3", url.Values{
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

func TestMessageRejectsAppendRemovedBeforePersistence(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	values := []kritui_db.PromptAppend{
		{ID: "keep", Name: "Keep", Text: "Keep it."},
		{ID: "gone", Name: "Gone", Text: "Gone."},
	}
	seedPromptAppends(t, database, values)
	// A settings save commits first and removes "gone", then the message is
	// submitted. Resolution happens inside the message transaction, so the
	// removed ID must be rejected against the current definitions.
	seedPromptAppends(t, database, values[:1])
	_, err := persistUserMessage(ctx, database, 1, "Use gone", nil, []string{}, []string{"gone", "keep"})
	if !errors.Is(err, errInvalidAppendSelection) {
		t.Fatalf("persistUserMessage() error = %v, want errInvalidAppendSelection", err)
	}

	var chats int
	if check := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chats); check != nil {
		t.Fatalf("count chats: %v", check)
	}
	if chats != 0 {
		t.Errorf("stored chats = %d, want 0", chats)
	}
	var messages int
	if check := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); check != nil {
		t.Fatalf("count messages: %v", check)
	}
	if messages != 0 {
		t.Errorf("stored messages = %d, want 0", messages)
	}
}

func TestMessageSelectionThenSettingsPruneNeverLeavesStaleAppends(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	keep := kritui_db.PromptAppend{ID: "keep", Name: "Keep", Text: "Keep it."}
	gone := kritui_db.PromptAppend{ID: "gone", Name: "Gone", Text: "Gone."}
	seedPromptAppends(t, database, []kritui_db.PromptAppend{keep, gone})
	// The message transaction resolves both appends and persists the selection
	// before the settings save that follows.
	selected, err := persistUserMessage(ctx, database, 90, "Use both", nil, []string{}, []string{"keep", "gone"})
	if err != nil {
		t.Fatalf("persistUserMessage(): %v", err)
	}
	if got := kritui_db.PromptAppendIDs(selected.appends); !slices.Equal(got, []string{"keep", "gone"}) {
		t.Errorf("selected append IDs = %v, want [keep gone]", got)
	}

	// A later settings save removes "gone"; SetPromptAppends prunes it from
	// the chat selection so no stale reference survives.
	seedPromptAppends(t, database, []kritui_db.PromptAppend{keep})
	ids, err := kritui_db.GetChatPromptAppendIDs(ctx, database, 90)
	if err != nil {
		t.Fatalf("GetChatPromptAppendIDs(): %v", err)
	}
	if !slices.Equal(ids, []string{"keep"}) {
		t.Errorf("chat prompt append IDs = %v, want [keep]", ids)
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
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore()), "/messages?chat=3", url.Values{
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
				database := openTestDatabase(t)
				return messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())
			},
			wantHTML: `<article class="message" role="alert">`,
		},
		{
			name:   "message edit",
			method: http.MethodPut,
			target: "/chats/1/messages/8",
			base:   "message=edited&model=test-model",
			limit:  maxMessageBodyBytes,
			newHandler: func(t *testing.T) http.Handler {
				database := openTestDatabase(t)
				if _, err := database.Exec(`
					INSERT INTO chats (id) VALUES (1);
					INSERT INTO messages (id, chat_id, position, role, content) VALUES (8, 1, 0, 'user', 'Original');
				`); err != nil {
					t.Fatalf("insert edit target: %v", err)
				}
				mux := http.NewServeMux()
				mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, newTestToolRegistry(t), newToolCallStore()))
				return mux
			},
			wantHTML: `id="message-edit-error-8"`,
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
	handler := messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())

	body, contentType := multipartBodyOfSize(t, maxMessagePostBytes)
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=invalid", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart body at limit returned 413: %s", response.Body.String())
	}

	body, contentType = multipartBodyOfSize(t, maxMessagePostBytes+1)
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
		response := serveRawForm(messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls), http.MethodPost, "/messages?chat=1", body)
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
		requestID, err := toolCalls.create(1, "", nil)
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
		if _, ok := toolCalls.claim(requestID, 1, "", nil); !ok {
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
		requestID, err := toolCalls.create(1, "", nil)
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
	requireContains(t, body, "Newest", "Middle", `before_id=2`, `class="history-loader"`, `hx-target="this"`, `hx-vals="js:{limit: Math.min(50, Math.max(1, Math.ceil((this.closest(&#39;.history-page&#39;)?.clientHeight ?? 0) / 80)))}`)
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
	if _, err := persistUserMessage(context.Background(), database, 1, "First title", nil, []string{}, nil); err != nil {
		t.Fatalf("store first chat: %v", err)
	}
	requireContains(t, loadHistory(), "First title")

	if _, err := persistUserMessage(context.Background(), database, 2, "Newest title", nil, []string{}, nil); err != nil {
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
		`aria-label="Settings"`,
		`tabindex="-1"`,
		`data-panel-initial-focus`,
		`action="/settings?chat=8"`,
		`method="post"`,
		`name="ntfy_form" value="1"`,
		`Save all`,
		`hx-target="#settings-page"`,
		`hx-swap="outerHTML"`,
		`id="mcp-server-list"`,
		`type="button"`,
		`hx-post="/settings?chat=8"`,
		`hx-target="#mcp-server-list"`,
		`hx-swap="beforeend"`,
		`hx-vals="{&#34;mcp_action&#34;:&#34;add&#34;}"`,
		`hx-include="#append-picker input[name='append']:checked, #tool-picker input[name='tool']:checked"`,
		`hx-status:4xx="target:#settings-page swap:outerHTML"`,
		`hx-status:5xx="target:#settings-page swap:outerHTML"`,
	)
	if count := strings.Count(response.Body.String(), ">Save all</button>"); count != 1 {
		t.Errorf("Save all button count = %d, want 1", count)
	}
	requireNotContains(t, response.Body.String(), "Save settings",
		"01 / Chat", "02 / Prompts", "03 / Delivery", "04 / Appearance", "05 / Integrations",
		"Conversation defaults")
	requireNotContains(t, response.Body.String(), "Save notifications")
	requireNotContains(t, response.Body.String(), `id="send-button"`, `id="settings-button"`)
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
		"default_append":     {"custom"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	values, err := kritui_db.GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("get prompt appends: %v", err)
	}
	want := []kritui_db.PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction.", EnabledByDefault: true}}
	if !slices.Equal(values, want) {
		t.Errorf("prompt appends = %#v, want %#v", values, want)
	}
	requireContains(t, response.Body.String(), `name="append_name_custom"`, `value="Custom"`, "Use custom instruction.", `name="default_append" value="custom" checked`, "Settings saved.")
}

func TestSettingsHandlerClearsDefaultPromptAppend(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	seedPromptAppends(t, database, []kritui_db.PromptAppend{{
		ID:               "custom",
		Name:             "Custom",
		Text:             "Use custom instruction.",
		EnabledByDefault: true,
	}})

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
	if len(values) != 1 || values[0].EnabledByDefault {
		t.Errorf("prompt appends = %#v, want one disabled append", values)
	}
	requireNotContains(t, response.Body.String(), `name="default_append" value="custom" checked`)
}

func TestSettingsHandlerRejectsUnknownDefaultPromptAppend(t *testing.T) {
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
		"default_append":     {"missing"},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), `unknown default prompt append &#34;missing&#34;`)
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

func TestSettingsHandlerRejectsMalformedAppendIDOnAdd(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	seeded := []kritui_db.PromptAppend{{ID: "existing", Name: "Existing", Text: "Keep it."}}
	seedPromptAppends(t, database, seeded)

	for _, malformed := range []string{"bad char!", strings.Repeat("a", 65)} {
		response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
			"model":           {"saved-model"},
			"max_tool_rounds": {"16"},
			"append_form":     {"1"},
			"append_id":       {"existing", malformed},
			"append_action":   {"add"},
		})

		if response.Code != http.StatusBadRequest {
			t.Errorf("status for %q = %d, want %d; body = %q", malformed, response.Code, http.StatusBadRequest, response.Body.String())
			continue
		}
		body := response.Body.String()
		for _, context := range []string{
			`name="append_name_` + malformed,
			`name="append_text_` + malformed,
			`id="append-name-` + malformed,
		} {
			if strings.Contains(body, context) {
				t.Errorf("add response rendered malformed ID %q into attribute %q", malformed, context)
			}
		}
		got, err := kritui_db.GetPromptAppends(context.Background(), database)
		if err != nil {
			t.Fatalf("get prompt appends: %v", err)
		}
		if !slices.Equal(got, seeded) {
			t.Errorf("prompt appends after rejected add with %q = %#v, want seeded %#v", malformed, got, seeded)
		}
	}
}

func TestSettingsHandlerRejectsMalformedRemoveAppendID(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	seeded := []kritui_db.PromptAppend{{ID: "existing", Name: "Existing", Text: "Keep it."}}
	seedPromptAppends(t, database, seeded)

	malformed := "bad char"
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":                {"saved-model"},
		"max_tool_rounds":      {"16"},
		"append_form":          {"1"},
		"append_id":            {"existing"},
		"append_name_existing": {"Existing"},
		"append_text_existing": {"Keep it."},
		"remove_append":        {malformed},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	body := response.Body.String()
	for _, context := range []string{
		`name="append_name_` + malformed,
		`name="append_text_` + malformed,
		`id="append-name-` + malformed,
	} {
		if strings.Contains(body, context) {
			t.Errorf("remove response rendered malformed ID %q into attribute %q", malformed, context)
		}
	}
	got, err := kritui_db.GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("get prompt appends: %v", err)
	}
	if !slices.Equal(got, seeded) {
		t.Errorf("prompt appends after malformed remove = %v, want seeded %v", got, seeded)
	}
}

func TestSettingsHandlerRefreshesAppendPickerAfterHTMXSave(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	form := url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"append_selection":   {"1"},
		"append":             {"custom"},
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
	requireContains(t, response.Body.String(), `id="append-picker"`, `hx-swap-oob="outerHTML"`, `value="custom" form="message-form" checked`, "Custom")
}

func TestSettingsHandlerReportsChatAppendsLoadFailureAfterHTMXSave(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		DROP TABLE chat_prompt_appends;
	`); err != nil {
		t.Fatalf("break chat prompt append storage: %v", err)
	}
	form := url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
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

func TestSettingsHandlerAppendActionPreservesPendingAPIKeyClear(t *testing.T) {
	database := openTestDatabase(t)
	if err := kritui_db.SaveNtfySettings(context.Background(), database, kritui_db.NtfySettingsUpdate{
		Endpoint:     "https://ntfy.example",
		Topic:        "kritui",
		APIKeyChange: kritui_db.NtfyReplaceAPIKey,
		APIKeyValue:  "secret-token",
	}); err != nil {
		t.Fatalf("seed ntfy settings: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":              {""},
		"max_tool_rounds":    {"invalid"},
		"append_form":        {"1"},
		"append_action":      {"add"},
		"ntfy_form":          {"1"},
		"ntfy_endpoint":      {"https://ntfy.example"},
		"ntfy_topic":         {"kritui"},
		"clear_ntfy_api_key": {"1"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`id="clear-ntfy-api-key" type="checkbox" name="clear_ntfy_api_key" value="1" hidden checked`,
		`id="ntfy-api-key" type="password" name="ntfy_api_key" autocomplete="new-password" hx-preserve="true" disabled`,
		`data-settings-clear-open hidden`,
		`data-settings-clear-pending role="status"`,
		"API key will be removed when settings are saved.",
	)
	requireNotContains(t, response.Body.String(), `data-settings-clear-pending role="status" hidden`)

	config, err := kritui_db.GetNtfyPublishConfig(context.Background(), database)
	if err != nil {
		t.Fatalf("get ntfy config: %v", err)
	}
	if config.APIKey != "secret-token" {
		t.Errorf("API key after append action = %q, want unchanged secret-token", config.APIKey)
	}
}

func TestSettingsHandlerRendersThemeOptions(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil)
	response := httptest.NewRecorder()

	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`<select id="theme" name="theme">`,
		`<option value="rose-pine" selected>Rose Pine Light</option>`,
		`<option value="nord">Nord</option>`,
		`<option value="tokyo-night">Tokyo Night</option>`,
		`<option value="og">OG</option>`,
		`data-theme-id="rose-pine"`,
		`data-theme-color="#faf4ed"`,
		`color-scheme:light`,
	)

	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "model",
		MaxToolRounds: 3,
		DefaultTools:  []string{"git"},
		Theme:         "tokyo-night",
	}); err != nil {
		t.Fatalf("seed theme: %v", err)
	}
	response = httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status after storing theme = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`<option value="rose-pine">Rose Pine Light</option>`,
		`<option value="rose-pine-dark">Rose Pine Dark</option>`,
		`<option value="tokyo-night" selected>Tokyo Night</option>`,
		`<option value="og">OG</option>`,
		`data-theme-id="tokyo-night"`,
		`data-theme-color="#1a1b26"`,
		`color-scheme:dark`,
	)
}

func TestSettingsHandlerStoresTheme(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"theme":           {"nord"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	theme, err := kritui_db.GetTheme(context.Background(), database)
	if err != nil {
		t.Fatalf("get theme: %v", err)
	}
	if theme != "nord" {
		t.Errorf("theme = %q, want nord", theme)
	}
	requireContains(t, response.Body.String(),
		`<option value="nord" selected>Nord</option>`,
		`data-theme-id="nord"`,
		`data-theme-color="#2e3440"`,
	)
}

func TestSettingsHandlerRejectsUnknownTheme(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"theme":           {"dracula"},
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Theme selection is invalid.")
	theme, err := kritui_db.GetTheme(context.Background(), database)
	if err != nil {
		t.Fatalf("get theme: %v", err)
	}
	if theme != "" {
		t.Errorf("theme after rejected save = %q, want empty", theme)
	}
}

func TestSettingsHandlerAppendActionPreservesTheme(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"append_form":     {"1"},
		"append_action":   {"add"},
		"theme":           {"nord"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`<option value="nord" selected>Nord</option>`,
		`data-theme-id="nord"`,
	)
	theme, err := kritui_db.GetTheme(context.Background(), database)
	if err != nil {
		t.Fatalf("get theme: %v", err)
	}
	if theme != "" {
		t.Errorf("theme after append action = %q, want unchanged empty", theme)
	}
}

func TestHomeHandlerRendersStoredTheme(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "model",
		MaxToolRounds: 3,
		DefaultTools:  []string{"git"},
		Theme:         "tokyo-night",
	}); err != nil {
		t.Fatalf("seed theme: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`data-theme="tokyo-night"`,
		`--color-background:#1a1b26`,
		`color-scheme:dark`,
		`name="theme-color" content="#1a1b26"`,
		`<option value="tokyo-night" selected>Tokyo Night</option>`,
	)
}

func TestHomeHandlerRendersDefaultThemeWhenUnset(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`data-theme="rose-pine"`,
		`--color-background:#faf4ed`,
		`color-scheme:light`,
		`name="theme-color" content="#faf4ed"`,
		`<option value="rose-pine" selected>Rose Pine Light</option>`,
	)
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
	seedDefaultEnabledTools(t, database, []string{"webfetch"})
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

func TestSettingsHandlerPreservesPromptAppendsWithoutAppendForm(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	appends := []kritui_db.PromptAppend{{ID: "custom", Name: "Custom", Text: "Use custom instruction."}}
	seedPromptAppends(t, database, appends)
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	got, err := kritui_db.GetPromptAppends(context.Background(), database)
	if err != nil {
		t.Fatalf("get prompt appends: %v", err)
	}
	if !slices.Equal(got, appends) {
		t.Errorf("prompt appends after save without append_form = %#v, want preserved %#v", got, appends)
	}
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

func TestSettingsHandlerRendersNtfyDestinationWithoutSecret(t *testing.T) {
	database := openTestDatabase(t)
	apiKey := "secret-token"
	if err := kritui_db.SaveNtfySettings(context.Background(), database, kritui_db.NtfySettingsUpdate{
		Endpoint:     "https://ntfy.example",
		Topic:        "kritui",
		APIKeyChange: kritui_db.NtfyReplaceAPIKey,
		APIKeyValue:  apiKey,
	}); err != nil {
		t.Fatalf("save ntfy settings: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	response := httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		`id="ntfy-settings"`,
		`name="ntfy_endpoint" value="https://ntfy.example"`,
		`name="ntfy_topic" value="kritui"`,
		"Clear stored key",
		`data-settings-clear-open`,
		`data-settings-clear-dialog`,
		`data-settings-clear-cancel`,
		`data-settings-clear-confirm`,
		`name="clear_ntfy_api_key" value="1" hidden`,
	)
	requireNotContains(t, response.Body.String(), apiKey, `name="ntfy_api_key" value=`)
}

func TestSettingsHandlerStoresAndPreservesNtfySecret(t *testing.T) {
	database := openTestDatabase(t)
	handler := settingsHandler(database, newTestToolRegistry(t))
	apiKey := "secret-token"

	response := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"32"},
		"ntfy_form":       {"1"},
		"ntfy_endpoint":   {"https://ntfy.example"},
		"ntfy_topic":      {"kritui"},
		"ntfy_api_key":    {apiKey},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Settings saved.", "Clear stored key")
	requireNotContains(t, response.Body.String(), apiKey, `name="ntfy_api_key" value=`)
	model, err := kritui_db.GetDefaultModel(context.Background(), database, "fallback")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if model != "saved-model" {
		t.Errorf("default model = %q, want saved-model", model)
	}
	rounds, err := kritui_db.GetMaxToolRounds(context.Background(), database, 1)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if rounds != 32 {
		t.Errorf("max tool rounds = %d, want 32", rounds)
	}

	config, err := kritui_db.GetNtfyPublishConfig(context.Background(), database)
	if err != nil {
		t.Fatalf("get ntfy config: %v", err)
	}
	if config.APIKey != apiKey {
		t.Errorf("stored API key = %q, want submitted key", config.APIKey)
	}

	response = postForm(t, handler, "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"32"},
		"ntfy_form":       {"1"},
		"ntfy_endpoint":   {"https://ntfy.example/new"},
		"ntfy_topic":      {"new-topic"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("preserve status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireNotContains(t, response.Body.String(), apiKey)
	config, err = kritui_db.GetNtfyPublishConfig(context.Background(), database)
	if err != nil {
		t.Fatalf("get ntfy config after preserve: %v", err)
	}
	if config.APIKey != apiKey || config.Endpoint != "https://ntfy.example/new" || config.Topic != "new-topic" {
		t.Errorf("config after preserve = %#v, want destination update and preserved key", config)
	}

	response = postForm(t, handler, "/settings?chat=8", url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"32"},
		"ntfy_form":          {"1"},
		"ntfy_endpoint":      {config.Endpoint},
		"ntfy_topic":         {config.Topic},
		"clear_ntfy_api_key": {"1"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "Clear stored key") {
		t.Error("clear response reports configured API key")
	}
	config, err = kritui_db.GetNtfyPublishConfig(context.Background(), database)
	if err != nil {
		t.Fatalf("get ntfy config after clear: %v", err)
	}
	if config.APIKey != "" {
		t.Errorf("API key after clear = %q, want empty", config.APIKey)
	}
}

func TestSettingsHandlerRejectsInvalidNtfyValuesWithoutSecret(t *testing.T) {
	database := openTestDatabase(t)
	secret := "secret-token"
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"32"},
		"ntfy_form":       {"1"},
		"ntfy_endpoint":   {"https://ntfy.example"},
		"ntfy_api_key":    {secret},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Endpoint and topic must both be set or empty.")
	requireNotContains(t, response.Body.String(), secret)
}

func TestHomeHandlerRendersNtfyDestinationWithoutSecret(t *testing.T) {
	database := openTestDatabase(t)
	apiKey := "secret-token"
	if err := kritui_db.SaveNtfySettings(context.Background(), database, kritui_db.NtfySettingsUpdate{
		Endpoint:     "https://ntfy.example",
		Topic:        "kritui",
		APIKeyChange: kritui_db.NtfyReplaceAPIKey,
		APIKeyValue:  apiKey,
	}); err != nil {
		t.Fatalf("save ntfy settings: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), `name="ntfy_endpoint" value="https://ntfy.example"`, `name="ntfy_topic" value="kritui"`)
	requireNotContains(t, response.Body.String(), apiKey)
}

func TestHomeHandlerAllocatesChatWithDefaultTools(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	seedDefaultEnabledTools(t, database, []string{"websearch"})

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", page.Code, http.StatusOK)
	}
	chatID := messageFormChatID(t, page.Body.String())

	names, err := kritui_db.GetChatTools(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
	}
	if len(names) != 1 || names[0] != "websearch" {
		t.Errorf("chat tools = %v, want [websearch]", names)
	}

	requireContains(t, page.Body.String(), `name="tool" value="websearch" form="message-form" checked`)
	requireNotContains(t, page.Body.String(), `name="tool" value="webfetch" form="message-form" checked`)
}

func TestHomeHandlerAllocatesChatWithoutDefaultToolsWhenUnset(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", page.Code, http.StatusOK)
	}
	chatID := messageFormChatID(t, page.Body.String())

	names, err := kritui_db.GetChatTools(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("chat tools = %v, want empty list", names)
	}

	requireNotContains(t, page.Body.String(),
		`name="tool" value="webfetch" form="message-form" checked`,
		`name="tool" value="websearch" form="message-form" checked`,
	)
}

func TestHomeHandlerAllocatesChatWithDefaultPromptAppends(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	appends := []kritui_db.PromptAppend{
		{ID: "enabled", Name: "Enabled", Text: "Enabled instruction.", EnabledByDefault: true},
		{ID: "disabled", Name: "Disabled", Text: "Disabled instruction."},
	}
	seedPromptAppends(t, database, appends)

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", page.Code, http.StatusOK)
	}
	chatID := messageFormChatID(t, page.Body.String())

	ids, err := kritui_db.GetChatPromptAppendIDs(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get chat prompt append IDs: %v", err)
	}
	if !slices.Equal(ids, []string{"enabled"}) {
		t.Errorf("chat prompt append IDs = %v, want [enabled]", ids)
	}
	requireContains(t, page.Body.String(), `name="append" value="enabled" form="message-form" checked`)
	requireNotContains(t, page.Body.String(), `name="append" value="disabled" form="message-form" checked`)
}

func TestHomeHandlerAllocatesChatWithoutDefaultPromptAppendsWhenUnset(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", page.Code, http.StatusOK)
	}
	chatID := messageFormChatID(t, page.Body.String())

	ids, err := kritui_db.GetChatPromptAppendIDs(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get chat prompt append IDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("chat prompt append IDs = %v, want empty list", ids)
	}
	requireNotContains(t, page.Body.String(),
		`name="append" value="link-check" form="message-form" checked`,
		`name="append" value="research" form="message-form" checked`,
	)
}

func TestSettingsHandlerStoresPreservesAndClearsMCPSecret(t *testing.T) {
	database := openTestDatabase(t)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
	handler := settingsHandler(database, newTestToolRegistry(t))
	const (
		serverID   = "mcp-0123456789abcdef"
		token      = "secret-token"
		configured = `placeholder="Stored token unchanged when blank"`
		clearInput = `name="clear_mcp_authorization" value="mcp-0123456789abcdef"`
	)
	storedToken := func() sql.NullString {
		t.Helper()
		var value sql.NullString
		if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&value); err != nil {
			t.Fatalf("query stored MCP token: %v", err)
		}
		return value
	}

	response := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":                         {"saved-model"},
		"max_tool_rounds":               {"16"},
		"mcp_form":                      {"1"},
		"mcp_id":                        {serverID},
		"mcp_name_" + serverID:          {"Example"},
		"mcp_url_" + serverID:           {"https://mcp.example"},
		"mcp_authorization_" + serverID: {token},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("store status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	requireContains(t, body,
		"Settings saved.",
		`>Example</legend>`,
		`name="mcp_id" value="`+serverID+`"`,
		configured,
		clearInput,
	)
	requireNotContains(t, body, token)
	if got := storedToken(); !got.Valid || got.String != token {
		t.Errorf("stored token after save = %#v, want %q", got, token)
	}

	response = postForm(t, handler, "/settings?chat=8", url.Values{
		"model":                         {"saved-model"},
		"max_tool_rounds":               {"16"},
		"mcp_form":                      {"1"},
		"mcp_id":                        {serverID},
		"mcp_name_" + serverID:          {"Example"},
		"mcp_url_" + serverID:           {"https://mcp.example"},
		"mcp_authorization_" + serverID: {""},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("preserve status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body = response.Body.String()
	requireContains(t, body, configured, clearInput)
	requireNotContains(t, body, token)
	if got := storedToken(); !got.Valid || got.String != token {
		t.Errorf("stored token after blank resave = %#v, want preserved %q", got, token)
	}

	response = postForm(t, handler, "/settings?chat=8", url.Values{
		"model":                   {"saved-model"},
		"max_tool_rounds":         {"16"},
		"mcp_form":                {"1"},
		"mcp_id":                  {serverID},
		"mcp_name_" + serverID:    {"Example"},
		"mcp_url_" + serverID:     {"https://mcp.example"},
		"clear_mcp_authorization": {serverID},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body = response.Body.String()
	requireNotContains(t, body, token, configured, clearInput)
	if got := storedToken(); got.Valid {
		t.Errorf("stored token after clear = %#v, want NULL", got)
	}
}

func TestSettingsHandlerMCPAddRendersOnlyNewEditor(t *testing.T) {
	database := openTestDatabase(t)
	const (
		serverID = "mcp-0123456789abcdef"
		token    = "secret-token"
	)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 16,
		MCPServers: []kritui_db.MCPServerUpdate{{
			Server:              kritui_db.MCPServer{ID: serverID, Name: "Existing", URL: "https://mcp.example"},
			AuthorizationChange: kritui_db.MCPReplaceAuthorization,
			AuthorizationValue:  token,
		}},
	}); err != nil {
		t.Fatalf("seed MCP server: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":                   {""},
		"max_tool_rounds":         {"invalid"},
		"mcp_form":                {"1"},
		"mcp_id":                  {serverID},
		"mcp_name_" + serverID:    {"Unsaved name"},
		"mcp_url_" + serverID:     {"https://unsaved.example/mcp"},
		"clear_mcp_authorization": {serverID},
		"mcp_action":              {"add"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	requireContains(t, body,
		">new server</legend>",
	)
	if count := strings.Count(body, `<fieldset class="settings-mcp-server">`); count != 1 {
		t.Errorf("rendered MCP editor count = %d, want 1", count)
	}
	if !regexp.MustCompile(`name="mcp_id" value="mcp-[0-9a-f]{16}"`).MatchString(body) {
		t.Errorf("response lacks valid generated MCP ID: %q", body)
	}
	requireNotContains(t, body,
		`id="settings-page"`,
		`>Existing</legend>`,
		`mcp_name_`+serverID,
		"Save settings",
		token,
	)

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM mcp_servers`).Scan(&count); err != nil {
		t.Fatalf("count mcp servers: %v", err)
	}
	if count != 1 {
		t.Errorf("mcp server count = %d, want 1; add action must not persist", count)
	}
	var stored sql.NullString
	if err := database.QueryRow(`SELECT authorization_token FROM mcp_servers WHERE id = ?`, serverID).Scan(&stored); err != nil {
		t.Fatalf("query stored MCP token: %v", err)
	}
	if !stored.Valid || stored.String != token {
		t.Errorf("stored token after add action = %#v, want preserved %q", stored, token)
	}
}

func TestSettingsHandlerHTMXSaveEmitsToolPickerWithMCPCapability(t *testing.T) {
	database := openTestDatabase(t)
	const (
		serverID   = "mcp-0123456789abcdef"
		capability = "mcp_server_0123456789abcdef"
	)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "stored-model",
		MaxToolRounds: 16,
		MCPServers: []kritui_db.MCPServerUpdate{{
			Server: kritui_db.MCPServer{ID: serverID, Name: "Example", URL: "https://mcp.example"},
		}},
	}); err != nil {
		t.Fatalf("seed MCP server: %v", err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")

	form := url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"default_tool":    {"webfetch", capability},
		"tool_selection":  {"1"},
		"tool":            {capability, "webfetch"},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings?chat=8", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		"Settings saved.",
		`id="tool-picker"`,
		`hx-swap-oob="outerHTML"`,
		`name="tool" value="webfetch" form="message-form" checked`,
		`name="tool" value="mcp_server_0123456789abcdef" form="message-form" checked`,
	)
	names, err := kritui_db.GetDefaultEnabledTools(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("get default tools: %v", err)
	}
	if !slices.Equal(names, []string{"webfetch", capability}) {
		t.Errorf("default tools = %v, want [webfetch %s]", names, capability)
	}
}

func TestPersistAndEditUserMessageRejectRemovedMCPCapability(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	const (
		serverID   = "mcp-0123456789abcdef"
		capability = "mcp_server_0123456789abcdef"
	)
	seedServer := func() {
		t.Helper()
		if err := kritui_db.SaveSettings(ctx, database, kritui_db.SettingsUpdate{
			Model:         "saved-model",
			MaxToolRounds: 16,
			MCPServers: []kritui_db.MCPServerUpdate{{
				Server: kritui_db.MCPServer{ID: serverID, Name: "Example", URL: "https://mcp.example"},
			}},
		}); err != nil {
			t.Fatalf("seed MCP server: %v", err)
		}
	}
	removeServer := func() {
		t.Helper()
		if err := kritui_db.SaveSettings(ctx, database, kritui_db.SettingsUpdate{
			Model:         "saved-model",
			MaxToolRounds: 16,
			MCPServers:    []kritui_db.MCPServerUpdate{},
		}); err != nil {
			t.Fatalf("remove MCP server: %v", err)
		}
	}

	seedServer()
	insertAcceptedUser(t, database, 1, "Original")
	removeServer()

	if _, err := persistUserMessage(ctx, database, 2, "Use mcp", nil, []string{capability}, nil); !errors.Is(err, errInvalidToolSelection) {
		t.Fatalf("persistUserMessage() error = %v, want errInvalidToolSelection", err)
	}
	var chats, messages int
	if err := database.QueryRow(`SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages)`).Scan(&chats, &messages); err != nil {
		t.Fatalf("count stored rows: %v", err)
	}
	if chats != 1 || messages != 1 {
		t.Errorf("stored rows = chats %d, messages %d, want unchanged 1, 1", chats, messages)
	}

	var messageID int64
	if err := database.QueryRow(`SELECT id FROM messages WHERE chat_id = 1`).Scan(&messageID); err != nil {
		t.Fatalf("query stored message: %v", err)
	}
	if err := editUserMessage(ctx, database, 1, messageID, "Edited", []string{capability}, nil); !errors.Is(err, errInvalidToolSelection) {
		t.Fatalf("editUserMessage() error = %v, want errInvalidToolSelection", err)
	}
	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, messageID).Scan(&content); err != nil {
		t.Fatalf("query unchanged message: %v", err)
	}
	if content != "Original" {
		t.Errorf("stored content = %q, want unchanged Original", content)
	}
	tools, err := kritui_db.GetChatTools(ctx, database, 1)
	if err != nil {
		t.Fatalf("get chat tools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("chat tools = %v, want unchanged empty", tools)
	}
}

func TestMigrateDatabaseCreatesMCPServersFromVersionFifteen(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`PRAGMA user_version = 15`); err != nil {
		t.Fatalf("initialize version fifteen database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}
	var columns int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM pragma_table_info('mcp_servers')
		WHERE name IN ('id', 'position', 'name', 'url', 'authorization_token')
	`).Scan(&columns); err != nil {
		t.Fatalf("inspect mcp servers columns: %v", err)
	}
	if columns != 5 {
		t.Fatalf("mcp servers column count = %d, want 5", columns)
	}

	ctx := context.Background()
	const (
		serverID   = "mcp-0123456789abcdef"
		capability = "mcp_server_0123456789abcdef"
		token      = "secret-token"
	)
	if _, err := database.Exec(`
		INSERT INTO mcp_servers (id, position, name, url, authorization_token)
		VALUES (?, 0, ?, ?, ?)
	`, serverID, "Example", "https://mcp.example", token); err != nil {
		t.Fatalf("store MCP server in migrated schema: %v", err)
	}
	servers, err := kritui_db.GetMCPServers(ctx, database)
	if err != nil {
		t.Fatalf("get MCP servers from migrated schema: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != serverID || !servers[0].AuthorizationConfigured {
		t.Fatalf("migrated MCP servers = %#v, want configured %q", servers, serverID)
	}
	configs, err := kritui_db.GetMCPServerConfigs(ctx, database, []string{capability})
	if err != nil {
		t.Fatalf("resolve MCP capability in migrated schema: %v", err)
	}
	if len(configs) != 1 || configs[0].ID != serverID || configs[0].AuthorizationToken != token {
		t.Errorf("resolved MCP configs = %#v, want stored token", configs)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("rerun migrations: %v", err)
	}
	servers, err = kritui_db.GetMCPServers(ctx, database)
	if err != nil {
		t.Fatalf("get MCP servers after rerun: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != serverID || !servers[0].AuthorizationConfigured {
		t.Errorf("MCP servers after rerun = %#v, want preserved configured %q", servers, serverID)
	}
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

func messageFormChatID(t *testing.T, body string) int64 {
	t.Helper()
	const prefix = `hx-post="/messages?chat=`
	_, value, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("message form action not found: %s", body)
	}
	value, _, ok = strings.Cut(value, `"`)
	if !ok {
		t.Fatalf("invalid message form action: %s", body)
	}
	return mustChatID(t, value)
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
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls), "/messages?chat=1", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(),
		"Hello",
		"selected-model",
		`class="message-copy-button"`,
		`class="message-edit-toggle"`,
		`hx-put="/chats/1/messages/1"`,
		`hx-post="/messages/complete?chat=1"`,
		`hx-trigger="load"`,
		`hx-sync="#messages:queue last"`,
		`hx-disable="#message-form button[type='submit']"`,
		`hx-sse:connect="/messages/tools?request=`,
		`hx-sse:close="close"`,
		`hx-config="sse.pauseOnBackground:false"`,
		`hx-vals="js:{client_timezone: Intl.DateTimeFormat().resolvedOptions().timeZone}"`,
		`id="completion-tools-`,
		`id="completion-`,
		`hx-swap-oob="outerHTML"`,
		`name="request" value="`,
		`name="tool" value="webfetch"`,
		`class="completion-network-error" role="alert" hidden`,
		"Failed to complete message. Check your connection and retry.",
		`hx-post="/messages/retry?chat=1"`,
		`data-completion-error-message`,
	)
	requireNotContains(t, response.Body.String(), "every 200ms", `type="hidden" name="message"`, `hx-ext="sse"`, `sse-connect=`, `sse-close=`, `sse-swap=`, `hx-disabled-elt=`)
	if count := strings.Count(response.Body.String(), `name="model" value="selected-model"`); count != 1 {
		t.Errorf("model input count = %d, want 1", count)
	}
	if count := strings.Count(response.Body.String(), `name="tool" value="webfetch"`); count != 1 {
		t.Errorf("tool input count = %d, want 1", count)
	}
}

func TestMessageEditHandlerTruncatesAndRestartsCompletion(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id, title) VALUES (7, 'Preserved title');
		INSERT INTO messages (id, chat_id, position, role, content, model) VALUES
			(10, 7, 0, 'user', 'Earlier question', NULL),
			(11, 7, 1, 'assistant', 'Earlier answer', 'old-model'),
			(12, 7, 2, 'user', 'Original question', NULL),
			(13, 7, 3, 'assistant', 'Discarded answer', 'old-model');
		INSERT INTO message_prompt_appends (message_id, message_role, position, text)
		VALUES (12, 'user', 0, 'Old append');
	`); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	toolCalls := newToolCallStore()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, newTestToolRegistry(t), toolCalls))
	response := serveRawForm(mux, http.MethodPut, "/chats/7/messages/12", url.Values{
		"message": {"Edited question"},
		"model":   {"selected-model"},
		"tool":    {"webfetch"},
		"append":  {"research"},
	}.Encode())

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if target := response.Header().Get("HX-Retarget"); target != "#messages" {
		t.Errorf("HX-Retarget = %q, want #messages", target)
	}
	if swap := response.Header().Get("HX-Reswap"); swap != "innerHTML" {
		t.Errorf("HX-Reswap = %q, want innerHTML", swap)
	}
	if trigger := response.Header().Get("HX-Trigger"); trigger != "kritui:message-edited" {
		t.Errorf("HX-Trigger = %q, want kritui:message-edited", trigger)
	}
	requireContains(t, response.Body.String(),
		"Earlier question",
		"Earlier answer",
		"Edited question",
		"selected-model",
		`hx-post="/messages/complete?chat=7"`,
		`hx-trigger="load"`,
		`hx-put="/chats/7/messages/12"`,
		`name="model" value="selected-model"`,
		`name="tool" value="webfetch"`,
	)
	requireNotContains(t, response.Body.String(), "Original question", "Discarded answer", `hx-swap-oob="outerHTML"`)

	rows, err := database.Query(`SELECT id, content FROM messages WHERE chat_id = 7 ORDER BY position`)
	if err != nil {
		t.Fatalf("query edited messages: %v", err)
	}
	defer rows.Close()
	var ids []int64
	var contents []string
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			t.Fatalf("scan edited message: %v", err)
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate edited messages: %v", err)
	}
	if !slices.Equal(ids, []int64{10, 11, 12}) || !slices.Equal(contents, []string{"Earlier question", "Earlier answer", "Edited question"}) {
		t.Errorf("edited messages = IDs %v content %v", ids, contents)
	}
	defaults := kritui_db.DefaultPromptAppends()
	var appendText, title string
	if err := database.QueryRow(`
		SELECT appends.text, chats.title
		FROM message_prompt_appends AS appends
		JOIN messages ON messages.id = appends.message_id
		JOIN chats ON chats.id = messages.chat_id
		WHERE messages.id = 12
	`).Scan(&appendText, &title); err != nil {
		t.Fatalf("query edited append and title: %v", err)
	}
	if appendText != defaults[1].Text || title != "Preserved title" {
		t.Errorf("edited append/title = %q, %q; want research append and preserved title", appendText, title)
	}
	tools, err := kritui_db.GetChatTools(context.Background(), database, 7)
	if err != nil {
		t.Fatalf("get edited chat tools: %v", err)
	}
	appends, err := kritui_db.GetChatPromptAppendIDs(context.Background(), database, 7)
	if err != nil {
		t.Fatalf("get edited chat appends: %v", err)
	}
	if !slices.Equal(tools, []string{"webfetch"}) || !slices.Equal(appends, []string{"research"}) {
		t.Errorf("edited options = tools %v appends %v", tools, appends)
	}
}

func TestMessageEditHandlerKeepsValidationErrorsInline(t *testing.T) {
	tests := []struct {
		name    string
		role    string
		form    url.Values
		status  int
		message string
	}{
		{name: "blank", role: "user", form: url.Values{"message": {" "}}, status: http.StatusBadRequest, message: "Message is required."},
		{name: "unknown append", role: "user", form: url.Values{"message": {"Edited"}, "append": {"missing"}}, status: http.StatusBadRequest, message: "Prompt append selection is invalid."},
		{name: "assistant", role: "assistant", form: url.Values{"message": {"Edited"}}, status: http.StatusConflict, message: "Only user messages can be edited."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDatabase(t)
			if _, err := database.Exec(`
				INSERT INTO chats (id) VALUES (1);
				INSERT INTO messages (id, chat_id, position, role, content) VALUES (8, 1, 0, ?, 'Original');
			`, test.role); err != nil {
				t.Fatalf("insert edit target: %v", err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, newTestToolRegistry(t), newToolCallStore()))
			response := serveRawForm(mux, http.MethodPut, "/chats/1/messages/8", test.form.Encode())

			requireHTMLErrorResponse(t, response, test.status, `id="message-edit-error-8"`, test.message)
			requireNotContains(t, response.Body.String(), `id="messages"`)
			var content string
			if err := database.QueryRow(`SELECT content FROM messages WHERE id = 8`).Scan(&content); err != nil {
				t.Fatalf("query unchanged target: %v", err)
			}
			if content != "Original" {
				t.Errorf("stored content = %q, want unchanged Original", content)
			}
		})
	}
}

func TestMessageEditHandlerRejectsActiveCompletion(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (id, chat_id, position, role, content) VALUES (8, 1, 0, 'user', 'Original');
	`); err != nil {
		t.Fatalf("insert edit target: %v", err)
	}
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	defer toolCalls.delete(requestID)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, newTestToolRegistry(t), toolCalls))
	response := serveRawForm(mux, http.MethodPut, "/chats/1/messages/8", "message=Edited")

	requireHTMLErrorResponse(t, response, http.StatusConflict, `id="message-edit-error-8"`, "A response is already in progress.")
	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = 8`).Scan(&content); err != nil {
		t.Fatalf("query unchanged target: %v", err)
	}
	if content != "Original" {
		t.Errorf("stored content = %q, want unchanged Original", content)
	}
}

func TestHTMXErrorsReturnTargetAppropriateHTML(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		database := openTestDatabase(t)
		response := httptest.NewRecorder()
		homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, httptest.NewRequest(http.MethodGet, "/?chat=invalid", nil))

		requireHTMLErrorResponse(t, response, http.StatusBadRequest, `<main>`, `id="messages"`, "A valid chat is required.")
	})

	t.Run("settings validation", func(t *testing.T) {
		database := openTestDatabase(t)
		t.Setenv("LLM_MODEL", "test-model")
		t.Setenv("LLM_ENDPOINT", "")
		request := httptest.NewRequest(http.MethodPost, "/settings?chat=1", strings.NewReader("%"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		settingsHandler(database, newTestToolRegistry(t))(response, request)

		requireHTMLErrorResponse(t, response, http.StatusBadRequest, `id="settings-page"`, `class="settings-main-form"`, "Invalid settings form.")
	})

	t.Run("settings storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		t.Setenv("LLM_MODEL", "test-model")
		t.Setenv("LLM_ENDPOINT", "")
		response := httptest.NewRecorder()
		settingsHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/settings?chat=1", nil))

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `id="settings-page"`, `class="settings-main-form"`, "Failed to load settings.")
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
		response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore()), "/messages?chat=1", url.Values{
			"message": {"Hello"},
			"model":   {"test-model"},
		})

		requireHTMLErrorResponse(t, response, http.StatusInternalServerError, `<article class="message" role="alert">`, "Failed to store message.")
	})

	t.Run("completion storage", func(t *testing.T) {
		database := openTestDatabase(t)
		database.Close()
		toolCalls := newToolCallStore()
		response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls, "/messages/complete?chat=1", url.Values{
			"model":   {"test-model"},
			"request": {newToolCallRequest(t, toolCalls, 1)},
		})

		requireHTMLErrorResponse(t, response, http.StatusOK, `class="message completion-error"`, "Failed to load conversation.")
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
	seedDefaultModel(t, database, "stored-default")
	toolCalls := newToolCallStore()
	response := postForm(t, messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), toolCalls), "/messages?chat=1", url.Values{"message": {"Hello"}})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "stored-default", `name="model" value="stored-default"`)
}

func TestMessageHandlerRejectsConcurrentCompletionForSameChat(t *testing.T) {
	database := openTestDatabase(t)
	registry := newTestToolRegistry(t)
	toolCalls := newToolCallStore()
	handler := messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls)

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
	replacementID, err := toolCalls.create(1, "", nil)
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
	entry := toolCalls.entries[requestID]
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
	tracker, ok := toolCalls.claim(requestID, 1, "", nil)
	if !ok {
		t.Fatal("claim tracker failed")
	}

	toolCalls.expireUnclaimed(requestID)
	if _, err := toolCalls.create(1, "", nil); !errors.Is(err, errChatCompletionActive) {
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
	replacementID, err := toolCalls.create(1, "", nil)
	if err != nil {
		t.Fatalf("reuse chat after completion: %v", err)
	}
	toolCalls.delete(replacementID)
}

func TestToolCallStoreFinishedTrackerExpires(t *testing.T) {
	toolCalls := newToolCallStore()
	toolCalls.finishedTTL = time.Millisecond
	requestID := newToolCallRequest(t, toolCalls, 1)
	tracker, ok := toolCalls.claim(requestID, 1, "selected-model", nil)
	if !ok {
		t.Fatal("claim tracker failed")
	}

	toolCalls.finish(requestID, "finished")
	waitForTestSignal(t, tracker.done, "finished tracker expiry")
	if _, ok := toolCalls.get(requestID); ok {
		t.Error("finished tracker remains stored after retention period")
	}
	if _, ok := toolCalls.active(1); ok {
		t.Error("finished tracker kept chat active")
	}
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
	completionResponse := postForm(t, messageCompletionHandler(database, registry, toolCalls, nil), "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {requestID},
	})
	if completionResponse.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", completionResponse.Code, http.StatusNoContent, completionResponse.Body.String())
	}
	waitForTestSignal(t, requestStarted, "active completion request")

	toolCalls.expireUnclaimed(requestID)
	replacement := postForm(t, messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls), "/messages?chat=1", url.Values{
		"message": {"Replacement question"},
		"model":   {"selected-model"},
	})
	if replacement.Code != http.StatusConflict {
		t.Fatalf("replacement status = %d, want %d; body = %q", replacement.Code, http.StatusConflict, replacement.Body.String())
	}

	close(releaseRequest)
	completionBody := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, completionBody, "Original answer.")
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
	tracker, ok := toolCalls.claim(requestID, 1, "", nil)
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

	body := response.Body.String()
	marker := `data: <hx-partial hx-target="#completion-tools-` + requestID + `" hx-swap="innerHTML">`
	if count := strings.Count(body, marker); count != 2 {
		t.Fatalf("tool update partial count = %d, want 2; body = %q", count, body)
	}
	if strings.Contains(body, "event: tools\n") || strings.Contains(body, "event: completion\n") {
		t.Fatalf("named tool/completion event in body: %q", body)
	}
	_, rest, ok := strings.Cut(body, marker)
	if !ok {
		t.Fatalf("response does not contain tool partial: %s", body)
	}
	runningEvent, rest, ok := strings.Cut(rest, marker)
	if !ok {
		t.Fatalf("response does not contain second tool partial: %s", body)
	}
	completedEvent, closeEvent, ok := strings.Cut(rest, "event: close\n")
	if !ok {
		t.Fatalf("response does not contain close event: %s", body)
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

func TestMessageToolStreamHandlerSendsHeartbeat(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	request := httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil)
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	response := newFlushingResponseRecorder()
	done := make(chan struct{})
	go func() {
		messageToolStreamHandlerWithHeartbeat(toolCalls, 10*time.Millisecond)(response, request)
		close(done)
	}()

	waitForTestSignal(t, response.flushes, "initial tool-call event")
	waitForTestSignal(t, response.flushes, "tool-call heartbeat")
	cancel()
	waitForTestSignal(t, done, "heartbeat stream cancellation")
	requireContains(t, response.Body.String(), ": keepalive\n\n")
}

func TestMessageToolStreamHandlerReplaysCompletedResult(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	if _, ok := toolCalls.claim(requestID, 1, "selected-model", []string{"webfetch"}); !ok {
		t.Fatal("claim tracker failed")
	}
	toolCalls.finish(requestID, `<article class="message">Finished.</article>`)

	response := newFlushingResponseRecorder()
	messageToolStreamHandler(toolCalls)(response, httptest.NewRequest(http.MethodGet, "/messages/tools?request="+requestID, nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), `data: <hx-partial hx-target="#completion-`+requestID+`" hx-swap="outerHTML">`, `<article class="message">Finished.</article>`)
	requireNotContains(t, response.Body.String(), "event: close\n")
	if _, ok := toolCalls.get(requestID); !ok {
		t.Error("finished tracker was not retained for reconnect")
	}
	replacementID, err := toolCalls.create(1, "", nil)
	if err != nil {
		t.Fatalf("finished completion kept chat active: %v", err)
	}
	toolCalls.delete(replacementID)
}

func TestMessageToolStreamHandlerClosesMissingTracker(t *testing.T) {
	response := newFlushingResponseRecorder()
	messageToolStreamHandler(newToolCallStore())(response, httptest.NewRequest(http.MethodGet, "/messages/tools?request=missing", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "event: close\n")
}

func TestMessageCompletionContinuesAfterRequestCancellation(t *testing.T) {
	database := openTestDatabase(t)
	insertAcceptedUser(t, database, 1, "Keep working")
	providerStarted := make(chan struct{})
	releaseProvider := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(providerStarted)
		select {
		case <-releaseProvider:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Still finished."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls, 1)
	form := url.Values{"model": {"selected-model"}, "request": {requestID}}
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}

	waitForTestSignal(t, providerStarted, "detached provider request")
	cancel()
	close(releaseProvider)
	result := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, result, "Still finished.")

	var stored string
	if err := database.QueryRow(`SELECT content FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&stored); err != nil {
		t.Fatalf("load detached completion: %v", err)
	}
	if stored != "Still finished." {
		t.Errorf("stored detached completion = %q, want %q", stored, "Still finished.")
	}
}

func TestHomeHandlerRestoresCompletionState(t *testing.T) {
	database := openTestDatabase(t)
	insertAcceptedUser(t, database, 1, "Question")
	registry := newTestToolRegistry(t)
	commands := newTestCommandRegistry(t, database)
	t.Setenv("LLM_KEY", "")
	t.Setenv("LLM_ENDPOINT", "")

	t.Run("active", func(t *testing.T) {
		toolCalls := newToolCallStore()
		requestID := newToolCallRequest(t, toolCalls, 1)
		if _, ok := toolCalls.claim(requestID, 1, "active-model", []string{"webfetch"}); !ok {
			t.Fatal("claim active completion")
		}
		response := httptest.NewRecorder()
		homeHandler(database, registry, commands, toolCalls)(response, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))

		requireContains(t, response.Body.String(), `class="message loading-message completion-active"`, `id="completion-`+requestID+`"`, `hx-sse:connect="/messages/tools?request=`+requestID+`"`, "active-model")
		requireNotContains(t, response.Body.String(), `hx-post="/messages/complete?chat=1"`)
	})

	t.Run("not started", func(t *testing.T) {
		toolCalls := newToolCallStore()
		requestID, err := toolCalls.create(1, "queued-model", []string{"websearch"})
		if err != nil {
			t.Fatalf("create queued completion: %v", err)
		}
		response := httptest.NewRecorder()
		homeHandler(database, registry, commands, toolCalls)(response, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))

		requireContains(t, response.Body.String(), `hx-post="/messages/complete?chat=1"`, `hx-sse:connect="/messages/tools?request=`+requestID+`"`, "queued-model", `name="tool" value="websearch"`)
	})

	t.Run("interrupted", func(t *testing.T) {
		response := httptest.NewRecorder()
		homeHandler(database, registry, commands, newToolCallStore())(response, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))

		requireContains(t, response.Body.String(), "Completion was interrupted. Retry the completion.", `hx-post="/messages/retry?chat=1"`)
		requireNotContains(t, response.Body.String(), `class="message loading-message completion-active"`)
	})

	t.Run("persisted result wins", func(t *testing.T) {
		if _, err := database.Exec(`INSERT INTO messages (chat_id, position, role, content, model) VALUES (1, 1, 'assistant', 'Finished answer.', 'finished-model')`); err != nil {
			t.Fatalf("insert finished answer: %v", err)
		}
		toolCalls := newToolCallStore()
		if _, err := toolCalls.create(1, "stale-active-model", nil); err != nil {
			t.Fatalf("create stale active completion: %v", err)
		}
		response := httptest.NewRecorder()
		homeHandler(database, registry, commands, toolCalls)(response, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))

		requireContains(t, response.Body.String(), "Finished answer.", "finished-model")
		requireNotContains(t, response.Body.String(), `class="message loading-message completion-active"`, "stale-active-model")
	})
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
		response := completeForm(t, handler, toolCalls, "/messages/complete?chat=1", form)

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

func TestMessageCompletionHandlerExpandsStoredPromptAppendsExactlyOnce(t *testing.T) {
	database := openTestDatabase(t)
	firstAppend := kritui_db.PromptAppend{ID: "append-one", Name: "First", Text: "First append text."}
	secondAppend := kritui_db.PromptAppend{ID: "append-two", Name: "Second", Text: "Second append text."}
	seedPromptAppends(t, database, []kritui_db.PromptAppend{firstAppend, secondAppend})

	registry, err := tools.NewRegistry(responsePersistenceTestTool{})
	if err != nil {
		t.Fatalf("create legacy registry: %v", err)
	}

	toolCalls := newToolCallStore()
	message := postForm(t, messageHandler(database, registry, newTestCommandRegistry(t, database), toolCalls), "/messages?chat=3", url.Values{
		"message": {"Original question"},
		"model":   {"selected-model"},
		"tool":    {"lookup"},
		"append":  {"append-one", "append-two"},
	})
	if message.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d; body = %q", message.Code, http.StatusOK, message.Body.String())
	}
	const requestField = `name="request" value="`
	_, requestID, ok := strings.Cut(message.Body.String(), requestField)
	if !ok {
		t.Fatalf("message response does not contain %q: %s", requestField, message.Body.String())
	}
	requestID, _, ok = strings.Cut(requestID, `"`)
	if !ok {
		t.Fatalf("message response request field is unterminated: %s", message.Body.String())
	}

	requestNumber := 0
	type providerRequest struct {
		Model    string        `json:"model"`
		Messages []llm.Message `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	requests := make(chan providerRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestNumber++
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		if requestNumber == 1 {
			arguments, _ := json.Marshal(map[string]string{"key": "value"})
			_, _ = w.Write([]byte(`{
				"model":"response-model",
				"choices":[{"message":{"role":"assistant","tool_calls":[{
					"id":"call-1",
					"type":"function",
					"function":{"name":"lookup","arguments":` + strconv.Quote(string(arguments)) + `}
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

	completion := completeForm(t, messageCompletionHandler(database, registry, toolCalls, nil), toolCalls,
		"/messages/complete?chat=3", url.Values{
			"model":   {"selected-model"},
			"request": {requestID},
			"tool":    {"lookup"},
		})
	if completion.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", completion.Code, http.StatusOK, completion.Body.String())
	}
	requireContains(t, completion.Body.String(), "Final answer.")

	const (
		original     = "Original question"
		firstText    = "First append text."
		secondText   = "Second append text."
		wantExpanded = original + "\n\n" + firstText + "\n\n" + secondText
	)

	for index := 0; index < 2; index++ {
		request := <-requests
		if len(request.Messages) < 2 || request.Messages[0].Role != "system" {
			t.Fatalf("provider request %d messages = %#v, want system lead", index+1, request.Messages)
		}
		requireContains(t, request.Messages[0].Content, "Current UTC datetime:")

		systemCount := 0
		originalCount := 0
		firstCount := 0
		secondCount := 0
		for _, message := range request.Messages {
			switch message.Role {
			case "system":
				systemCount++
			case "user":
				originalCount += strings.Count(message.Content, original)
				firstCount += strings.Count(message.Content, firstText)
				secondCount += strings.Count(message.Content, secondText)
				if message.Content != wantExpanded {
					t.Errorf("request %d user message = %q, want %q", index+1, message.Content, wantExpanded)
				}
			}
		}
		if index == 0 {
			if systemCount != 1 {
				t.Errorf("request %d system message count = %d, want 1", index+1, systemCount)
			}
		}
		if originalCount != 1 {
			t.Errorf("request %d original text occurrences = %d, want 1", index+1, originalCount)
		}
		if firstCount != 1 {
			t.Errorf("request %d first append occurrences = %d, want 1", index+1, firstCount)
		}
		if secondCount != 1 {
			t.Errorf("request %d second append occurrences = %d, want 1", index+1, secondCount)
		}
	}
	if requestNumber != 2 {
		t.Errorf("provider request count = %d, want 2", requestNumber)
	}

	var storedContent string
	if err := database.QueryRow(`
		SELECT content FROM messages WHERE chat_id = 3 AND role = 'user'
	`).Scan(&storedContent); err != nil {
		t.Fatalf("query stored user message: %v", err)
	}
	if storedContent != original {
		t.Errorf("stored user content = %q, want raw %q", storedContent, original)
	}
	storedMessages, err := kritui_db.GetMessages(context.Background(), database, 3)
	if err != nil {
		t.Fatalf("get stored messages: %v", err)
	}
	var snapshots []string
	for _, message := range storedMessages {
		if message.Role == "user" {
			snapshots = message.PromptAppendTexts
			break
		}
	}
	if !slices.Equal(snapshots, []string{firstText, secondText}) {
		t.Errorf("stored append snapshots = %v, want %v", snapshots, []string{firstText, secondText})
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
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls, "/messages/complete?chat=1", form)

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

func TestMessageCompletionHandlerNotifiesAfterPersistence(t *testing.T) {
	database := openTestDatabase(t)
	observations := make(chan struct {
		method        string
		path          string
		authorization string
		title         string
		message       string
		assistants    int
	}, 1)
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read ntfy body: %v", err)
		}
		var assistants int
		if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&assistants); err != nil {
			t.Errorf("count persisted assistants: %v", err)
		}
		observations <- struct {
			method        string
			path          string
			authorization string
			title         string
			message       string
			assistants    int
		}{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			title:         r.Header.Get("Title"),
			message:       string(body),
			assistants:    assistants,
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ntfyServer.Close()

	if err := kritui_db.SaveNtfySettings(context.Background(), database, kritui_db.NtfySettingsUpdate{
		Endpoint:     ntfyServer.URL,
		Topic:        "responses",
		APIKeyChange: kritui_db.NtfyReplaceAPIKey,
		APIKeyValue:  "ntfy-secret",
	}); err != nil {
		t.Fatalf("save ntfy settings: %v", err)
	}

	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Answer"},"finish_reason":"stop"}]
		}`))
	}))
	defer modelServer.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", modelServer.URL)

	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Question")
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	select {
	case observation := <-observations:
		if observation.method != http.MethodPost || observation.path != "/responses" {
			t.Errorf("ntfy request = %s %q, want POST /responses", observation.method, observation.path)
		}
		if observation.authorization != "Bearer ntfy-secret" {
			t.Errorf("ntfy Authorization = %q, want bearer token", observation.authorization)
		}
		if observation.title != "Kritui response ready" {
			t.Errorf("ntfy title = %q, want response title", observation.title)
		}
		if observation.message != "Chat 1 has a response." {
			t.Errorf("ntfy message = %q, want metadata message", observation.message)
		}
		if observation.assistants != 1 {
			t.Errorf("assistant count during notification = %d, want persisted response", observation.assistants)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ntfy notification")
	}
}

func stringPointer(value string) *string {
	return &value
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
	requestID := newToolCallRequest(t, toolCalls, 1)
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=1", strings.NewReader(url.Values{
		"model":   {"selected-model"},
		"request": {requestID},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	registry := newTestToolRegistry(t)
	messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}

	waitForTestSignal(t, modelStarted, "model request")
	insertAcceptedUser(t, database, 1, "Concurrent question")
	close(releaseModel)
	completionBody := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, completionBody,
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
	response := completeForm(t, messageCompletionHandler(database, registry, toolCalls, nil), toolCalls, "/messages/complete?chat=1", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
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
	if _, ok := toolCalls.active(1); ok {
		t.Error("failed completion kept chat active")
	}

	retryResponse := postForm(t, messageRetryHandler(database, registry, toolCalls), "/messages/retry?chat=1", url.Values{
		"model": {"selected-model"},
		"tool":  {"webfetch"},
	})

	if retryResponse.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d; body = %q", retryResponse.Code, http.StatusOK, retryResponse.Body.String())
	}
	requireContains(t, retryResponse.Body.String(), `hx-post="/messages/complete?chat=1"`, `hx-sse:connect="/messages/tools?request=`, `hx-trigger="load"`)

	var messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1`).Scan(&messages); err != nil {
		t.Fatalf("count messages after retry: %v", err)
	}
	if messages != 1 {
		t.Errorf("message count after retry = %d, want persisted user only", messages)
	}
}

func TestMessageCompletionPersistsSuccessfulEndpointType(t *testing.T) {
	database := openTestDatabase(t)
	insertAcceptedUser(t, database, 1, "First question")

	var paths []string
	chatRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unsupported protocol", http.StatusInternalServerError)
			return
		}
		chatRequests++
		_, _ = fmt.Fprintf(w, `{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Answer %d"},"finish_reason":"stop"}]
		}`, chatRequests)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/responses")

	toolCalls := newToolCallStore()
	handler := messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)
	first := completeForm(t, handler, toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first completion status = %d, want %d; body = %q", first.Code, http.StatusOK, first.Body.String())
	}
	if endpointType, err := kritui_db.GetModelEndpointType(context.Background(), database, "selected-model"); err != nil {
		t.Fatalf("get stored endpoint type: %v", err)
	} else if endpointType != llm.EndpointChatCompletions {
		t.Errorf("stored endpoint type = %q, want chat_completions", endpointType)
	}

	insertAcceptedUser(t, database, 1, "Second question")
	second := completeForm(t, handler, toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})
	if second.Code != http.StatusOK {
		t.Fatalf("second completion status = %d, want %d; body = %q", second.Code, http.StatusOK, second.Body.String())
	}

	wantPaths := []string{"/v1/responses", "/v1/chat/messages", "/v1/chat/completions", "/v1/chat/completions"}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("provider paths = %v, want %v", paths, wantPaths)
	}
	requireContains(t, first.Body.String(), "Answer 1")
	requireContains(t, second.Body.String(), "Answer 2")
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
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls, "/messages/complete?chat=1", form)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
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
	if _, ok := toolCalls.claim(requestID, 1, "", nil); !ok {
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

	messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
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
	body := waitForCompletionResult(t, toolCalls, requestID)
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
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(reloadResponse, reloadRequest)
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
	messageCompletionHandler(database, registry, toolCalls, nil)(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
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
	body := waitForCompletionResult(t, toolCalls, requestID)
	requireContains(t, body,
		`class="tool-call tool-error"`,
		`class="tool-call-error"`,
		"current events",
		"websearch: SearXNG returned no results",
		"Search unavailable.",
	)

	t.Setenv("LLM_ENDPOINT", "")
	reloadResponse := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(reloadResponse, httptest.NewRequest(http.MethodGet, "/?chat=1", nil))
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
	firstResponse := completeForm(t, messageCompletionHandler(database, registry, toolCalls, nil), toolCalls, "/messages/complete?chat=1", url.Values{
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
	if _, err := persistUserMessage(context.Background(), database, 1, "Second question", nil, []string{"lookup"}, nil); err != nil {
		t.Fatalf("store second user message: %v", err)
	}

	toolCalls = newToolCallStore()
	secondResponse := completeForm(t, messageCompletionHandler(database, registry, toolCalls, nil), toolCalls, "/messages/complete?chat=1", url.Values{
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

func TestMigrateDatabaseAddsUndoSequence(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			position INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT ''
		) STRICT;
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'existing');
		PRAGMA user_version = 8;
	`); err != nil {
		t.Fatalf("initialize version eight database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version, columns int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'undo_sequence'`).Scan(&columns); err != nil {
		t.Fatalf("inspect undo sequence column: %v", err)
	}
	var sequence sql.NullInt64
	if err := database.QueryRow(`SELECT undo_sequence FROM messages WHERE id = 1`).Scan(&sequence); err != nil {
		t.Fatalf("read existing message undo sequence: %v", err)
	}
	if version != len(databaseMigrations) || columns != 1 || sequence.Valid {
		t.Errorf("migrated database = version %d, columns %d, sequence %#v", version, columns, sequence)
	}
}

func TestMigrateDatabaseAddsThemeSetting(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			default_model TEXT
		) STRICT;
		INSERT INTO settings (id, default_model) VALUES (1, 'kept-model');
		PRAGMA user_version = 12;
	`); err != nil {
		t.Fatalf("initialize version twelve database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version, columns int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('settings') WHERE name = 'theme'`).Scan(&columns); err != nil {
		t.Fatalf("inspect theme column: %v", err)
	}
	var theme sql.NullString
	var model string
	if err := database.QueryRow(`SELECT theme, default_model FROM settings WHERE id = 1`).Scan(&theme, &model); err != nil {
		t.Fatalf("read settings after migration: %v", err)
	}
	if version != len(databaseMigrations) || columns != 1 || theme.Valid || model != "kept-model" {
		t.Errorf("migrated settings = version %d, theme columns %d, theme %#v, model %q", version, columns, theme, model)
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'dracula' WHERE id = 1`); err == nil {
		t.Error("store invalid theme after migration error = nil, want constraint rejection")
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'nord' WHERE id = 1`); err != nil {
		t.Errorf("store valid theme after migration error: %v", err)
	}
}

func TestMigrateDatabaseRebuildsSettingsForRosePineDarkTheme(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			default_model TEXT,
			max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
			default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
			prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
			ntfy_endpoint TEXT,
			ntfy_topic TEXT,
			ntfy_api_key TEXT,
			theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'nord', 'tokyo-night'))
		) STRICT;
		INSERT INTO settings (id, default_model, max_tool_rounds, default_tools_configured, prompt_appends_configured, ntfy_endpoint, ntfy_topic, theme)
			VALUES (1, 'kept-model', 9, 1, 1, 'https://ntfy.example', 'topic', 'rose-pine');
		PRAGMA user_version = 13;
	`); err != nil {
		t.Fatalf("initialize version thirteen database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}
	var theme sql.NullString
	var model string
	var maxToolRounds int
	var ntfyEndpoint, ntfyTopic string
	if err := database.QueryRow(`
		SELECT theme, default_model, max_tool_rounds, ntfy_endpoint, ntfy_topic
		FROM settings WHERE id = 1
	`).Scan(&theme, &model, &maxToolRounds, &ntfyEndpoint, &ntfyTopic); err != nil {
		t.Fatalf("read settings after migration: %v", err)
	}
	if !theme.Valid || theme.String != "rose-pine" || model != "kept-model" || maxToolRounds != 9 || ntfyEndpoint != "https://ntfy.example" || ntfyTopic != "topic" {
		t.Errorf("migrated settings = theme %#v, model %q, rounds %d, endpoint %q, topic %q, want values preserved",
			theme, model, maxToolRounds, ntfyEndpoint, ntfyTopic)
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'dracula' WHERE id = 1`); err == nil {
		t.Error("store invalid theme after migration error = nil, want constraint rejection")
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'rose-pine-dark' WHERE id = 1`); err != nil {
		t.Errorf("store rose-pine-dark after migration error: %v", err)
	}
	stored, err := kritui_db.GetTheme(context.Background(), database)
	if err != nil {
		t.Fatalf("get theme after migration: %v", err)
	}
	if stored != "rose-pine-dark" {
		t.Errorf("stored theme = %q, want rose-pine-dark", stored)
	}
}

func TestMigrateDatabaseRebuildsSettingsForOGTheme(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			default_model TEXT,
			max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
			default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
			prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
			ntfy_endpoint TEXT,
			ntfy_topic TEXT,
			ntfy_api_key TEXT,
			theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'rose-pine-dark', 'nord', 'tokyo-night'))
		) STRICT;
		INSERT INTO settings (id, default_model, max_tool_rounds, default_tools_configured, prompt_appends_configured, ntfy_endpoint, ntfy_topic, theme)
			VALUES (1, 'kept-model', 7, 1, 1, 'https://ntfy.example', 'topic', 'nord');
		PRAGMA user_version = 14;
	`); err != nil {
		t.Fatalf("initialize version fourteen database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}
	var theme sql.NullString
	var model string
	var maxToolRounds int
	var ntfyEndpoint, ntfyTopic string
	if err := database.QueryRow(`
		SELECT theme, default_model, max_tool_rounds, ntfy_endpoint, ntfy_topic
		FROM settings WHERE id = 1
	`).Scan(&theme, &model, &maxToolRounds, &ntfyEndpoint, &ntfyTopic); err != nil {
		t.Fatalf("read settings after migration: %v", err)
	}
	if !theme.Valid || theme.String != "nord" || model != "kept-model" || maxToolRounds != 7 || ntfyEndpoint != "https://ntfy.example" || ntfyTopic != "topic" {
		t.Errorf("migrated settings = theme %#v, model %q, rounds %d, endpoint %q, topic %q, want values preserved",
			theme, model, maxToolRounds, ntfyEndpoint, ntfyTopic)
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'dracula' WHERE id = 1`); err == nil {
		t.Error("store invalid theme after migration error = nil, want constraint rejection")
	}
	if _, err := database.Exec(`UPDATE settings SET theme = 'og' WHERE id = 1`); err != nil {
		t.Errorf("store og after migration error: %v", err)
	}
	stored, err := kritui_db.GetTheme(context.Background(), database)
	if err != nil {
		t.Fatalf("get theme after migration: %v", err)
	}
	if stored != "og" {
		t.Errorf("stored theme = %q, want og", stored)
	}
}

func TestMigrateDatabaseAddsNtfySettings(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	if _, err := database.Exec(`
		CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK (id = 1)) STRICT;
		INSERT INTO settings (id) VALUES (1);
		PRAGMA user_version = 9;
	`); err != nil {
		t.Fatalf("initialize version nine database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var columns int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM pragma_table_info('settings')
		WHERE name IN ('ntfy_endpoint', 'ntfy_topic', 'ntfy_api_key')
	`).Scan(&columns); err != nil {
		t.Fatalf("inspect ntfy columns: %v", err)
	}
	if columns != 3 {
		t.Errorf("ntfy column count = %d, want 3", columns)
	}
	if _, err := kritui_db.GetNtfySettings(context.Background(), database); err != nil {
		t.Fatalf("get migrated ntfy settings: %v", err)
	}
}

func TestMigrateDatabaseAddsModelEndpointPreferences(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.Exec(`PRAGMA user_version = 10`); err != nil {
		t.Fatalf("initialize version ten database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, "model", llm.EndpointMessages); err != nil {
		t.Fatalf("set migrated model endpoint type: %v", err)
	}
	endpointType, err := kritui_db.GetModelEndpointType(context.Background(), database, "model")
	if err != nil {
		t.Fatalf("get migrated model endpoint type: %v", err)
	}
	if endpointType != llm.EndpointMessages {
		t.Errorf("migrated endpoint type = %q, want messages", endpointType)
	}
}

func TestMigrateDatabaseNormalizesVersionSevenData(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	if _, err := database.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE settings (name TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			tools TEXT NOT NULL DEFAULT '[]',
			appends TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT;
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			position INTEGER NOT NULL CHECK (position >= 0),
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			prompt_appends TEXT,
			model TEXT,
			total_tokens INTEGER,
			cost REAL,
			tool_calls TEXT,
			tool_call_id TEXT,
			provider_metadata TEXT,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			UNIQUE (chat_id, position)
		) STRICT;
		INSERT INTO settings (name, value) VALUES
			('default_model', 'legacy-model'),
			('max_tool_rounds', '9'),
			('default_tools', '["webfetch","git"]'),
			('prompt_appends', '[{"id":"research","name":"Research","text":"Research deeply.","enabled_by_default":true}]');
		INSERT INTO chats (id, title, tools, appends, created_at, updated_at)
		VALUES (7, 'legacy chat', '["webfetch"]', '["research"]', '2026-01-01T00:00:00.000Z', '2026-01-02T00:00:00.000Z');
		INSERT INTO messages
			(id, chat_id, position, role, content, prompt_appends, model, total_tokens, cost, tool_calls, tool_call_id, provider_metadata)
		VALUES
			(10, 7, 0, 'user', 'question', '["Research deeply."]', NULL, NULL, NULL, NULL, NULL, NULL),
			(11, 7, 1, 'assistant', '', NULL, 'legacy-model', 12, 0.25,
			 '[{"id":"call-1","type":"function","function":{"name":"webfetch","arguments":"opaque-provider-text"}}]',
			 NULL, '{"responses_output":[{"type":"reasoning","id":"reasoning-1","encrypted_content":"opaque"}]}'),
			(12, 7, 2, 'tool', 'result', NULL, NULL, NULL, NULL, NULL, 'call-1', NULL);
		PRAGMA user_version = 7;
	`); err != nil {
		t.Fatalf("initialize version seven database: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}

	ctx := context.Background()
	if got, err := kritui_db.GetDefaultEnabledTools(ctx, database, nil); err != nil {
		t.Fatalf("get migrated default tools: %v", err)
	} else if !slices.Equal(got, []string{"webfetch", "git"}) {
		t.Errorf("migrated default tools = %v, want [webfetch git]", got)
	}
	if got, err := kritui_db.GetMaxToolRounds(ctx, database, 1); err != nil {
		t.Fatalf("get migrated max tool rounds: %v", err)
	} else if got != 9 {
		t.Errorf("migrated max tool rounds = %d, want 9", got)
	}
	if got, err := kritui_db.GetPromptAppends(ctx, database); err != nil {
		t.Fatalf("get migrated prompt appends: %v", err)
	} else if !slices.Equal(got, []kritui_db.PromptAppend{{
		ID: "research", Name: "Research", Text: "Research deeply.", EnabledByDefault: true,
	}}) {
		t.Errorf("migrated prompt appends = %#v", got)
	}
	if got, err := kritui_db.GetChatPromptAppendIDs(ctx, database, 7); err != nil {
		t.Fatalf("get migrated chat prompt appends: %v", err)
	} else if !slices.Equal(got, []string{"research"}) {
		t.Errorf("migrated chat prompt appends = %v, want [research]", got)
	}

	messages, err := kritui_db.GetMessages(ctx, database, 7)
	if err != nil {
		t.Fatalf("get migrated messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("migrated message count = %d, want 3", len(messages))
	}
	if !slices.Equal(messages[0].PromptAppendTexts, []string{"Research deeply."}) {
		t.Errorf("migrated prompt append snapshots = %v", messages[0].PromptAppendTexts)
	}
	if len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].ID != "call-1" || messages[1].ToolCalls[0].Function.Name != "webfetch" || messages[1].ToolCalls[0].Function.Arguments != "opaque-provider-text" {
		t.Errorf("migrated tool calls = %#v", messages[1].ToolCalls)
	}
	outputs := messages[1].ProviderMetadata.ResponsesOutput()
	if len(outputs) != 1 || !strings.Contains(string(outputs[0]), `"reasoning-1"`) {
		t.Errorf("migrated provider output = %s", outputs)
	}
	for _, removedColumn := range []string{"tools", "appends", "prompt_appends", "tool_calls", "provider_metadata"} {
		var count int
		err := database.QueryRow(`
			SELECT COUNT(*)
			FROM pragma_table_info(CASE WHEN ? IN ('tools', 'appends') THEN 'chats' ELSE 'messages' END)
			WHERE name = ?
		`, removedColumn, removedColumn).Scan(&count)
		if err != nil {
			t.Fatalf("inspect removed column %q: %v", removedColumn, err)
		}
		if count != 0 {
			t.Errorf("legacy column %q still exists", removedColumn)
		}
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

func newTestCommandRegistry(t *testing.T, database *sql.DB) *commands.Registry {
	t.Helper()

	registry, err := newCommandRegistry(database)
	if err != nil {
		t.Fatalf("newCommandRegistry() error: %v", err)
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

func completeForm(t *testing.T, handler http.Handler, toolCalls *toolCallStore, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	started := postForm(t, handler, target, form)
	if started.Code != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", started.Code, http.StatusNoContent, started.Body.String())
	}

	response := httptest.NewRecorder()
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Body.WriteString(waitForCompletionResult(t, toolCalls, form.Get("request")))
	return response
}

func waitForCompletionResult(t *testing.T, toolCalls *toolCallStore, requestID string) string {
	t.Helper()
	tracker, ok := toolCalls.get(requestID)
	if !ok {
		t.Fatalf("completion tracker %q is missing", requestID)
	}
	t.Cleanup(func() { toolCalls.delete(requestID) })

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		_, _, _, terminal, finished, updates := tracker.streamSnapshot()
		if finished {
			return terminal
		}
		select {
		case <-updates:
		case <-tracker.done:
			t.Fatalf("completion tracker %q closed without a result", requestID)
		case <-timeout.C:
			t.Fatalf("timed out waiting for completion %q", requestID)
		}
	}
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

type testUpload struct {
	name, contentType string
	data              []byte
}

func testImageBytes(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	image := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			image.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
		}
	}
	var body bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&body, image, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&body, image)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return body.Bytes()
}

func testWebPBytes(t *testing.T) []byte {
	t.Helper()
	const encoded = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode WebP fixture: %v", err)
	}
	return data
}

func multipartImageRequest(t *testing.T, message string, files []testUpload) ([]byte, string) {
	return multipartImageRequestField(t, "image", filesWithMessage(message, files))
}

func filesWithMessage(message string, files []testUpload) []testUpload {
	return append([]testUpload{{name: "__message__", data: []byte(message)}}, files...)
}

func multipartImageRequestField(t *testing.T, field string, files []testUpload) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		if file.name == "__message__" {
			if err := writer.WriteField("message", string(file.data)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, file.name))
		header.Set("Content-Type", file.contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteField("model", "vision-model"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
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

	requestID, err := toolCalls.create(chatID, "", nil)
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

func seedChat(t *testing.T, database *sql.DB, title string, tools, appends []string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := database.QueryRowContext(ctx, `INSERT INTO chats (title) VALUES (?) RETURNING id`, title).Scan(&id); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if err := kritui_db.UpsertChat(ctx, database, id, title, tools, appends); err != nil {
		t.Fatalf("store chat options: %v", err)
	}
	return id
}

func seedDefaultModel(t *testing.T, database *sql.DB, model string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE settings SET default_model = ? WHERE id = 1`, model); err != nil {
		t.Fatalf("seed default model: %v", err)
	}
}

func seedDefaultEnabledTools(t *testing.T, database *sql.DB, names []string) {
	t.Helper()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin default tools seed: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM default_tools`); err != nil {
		t.Fatalf("clear default tools: %v", err)
	}
	for position, name := range names {
		if _, err := tx.Exec(`INSERT INTO default_tools (position, name) VALUES (?, ?)`, position, name); err != nil {
			t.Fatalf("seed default tool %d: %v", position, err)
		}
	}
	if _, err := tx.Exec(`UPDATE settings SET default_tools_configured = 1 WHERE id = 1`); err != nil {
		t.Fatalf("mark default tools configured: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit default tools seed: %v", err)
	}
}

func seedPromptAppends(t *testing.T, database *sql.DB, values []kritui_db.PromptAppend) {
	t.Helper()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin prompt appends seed: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM prompt_appends`); err != nil {
		t.Fatalf("clear prompt appends: %v", err)
	}
	for position, value := range values {
		if _, err := tx.Exec(`
			INSERT INTO prompt_appends (id, position, name, text, enabled_by_default)
			VALUES (?, ?, ?, ?, ?)
		`, value.ID, position, value.Name, value.Text, value.EnabledByDefault); err != nil {
			t.Fatalf("seed prompt append %q: %v", value.ID, err)
		}
	}
	if _, err := tx.Exec(`
		DELETE FROM chat_prompt_appends
		WHERE prompt_append_id NOT IN (SELECT id FROM prompt_appends)
	`); err != nil {
		t.Fatalf("prune seeded chat appends: %v", err)
	}
	if _, err := tx.Exec(`UPDATE settings SET prompt_appends_configured = 1 WHERE id = 1`); err != nil {
		t.Fatalf("mark prompt appends configured: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prompt appends seed: %v", err)
	}
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

	chats, err := kritui_db.GetChatsPage(ctx, database, "", 0, 100)
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
	seedDefaultModel(t, database, "migrated-model")
	model, err := kritui_db.GetDefaultModel(ctx, database, "fallback")
	if err != nil {
		t.Fatalf("get migrated setting: %v", err)
	}
	if model != "migrated-model" {
		t.Errorf("migrated setting = %q, want migrated-model", model)
	}
}
