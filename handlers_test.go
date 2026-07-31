package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

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
	for _, content := range []string{"Earlier question", "Earlier answer"} {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("response does not contain %q", content)
		}
	}
	if strings.Contains(response.Body.String(), "What would you like to discuss?") {
		t.Error("response contains welcome prompt for stored chat")
	}
	if strings.Contains(response.Body.String(), "begin a convo...") {
		t.Error("response contains empty-chat prompt for stored chat")
	}
	if !strings.Contains(response.Body.String(), "<strong>stored-model</strong>") {
		t.Error("response does not label assistant message with model")
	}
	if strings.Contains(response.Body.String(), "<strong>assistant</strong>") {
		t.Error("response labels assistant message with role")
	}
}

func TestHomeHandlerRendersEmptyChatPrompt(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if strings.Contains(response.Body.String(), "What would you like to discuss?") {
		t.Error("response contains welcome prompt")
	}
	if !strings.Contains(response.Body.String(), "begin a convo...") {
		t.Error("response does not contain empty-chat prompt")
	}
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
	for _, model := range []string{"model-a", "model-b"} {
		if !strings.Contains(response.Body.String(), `value="`+model+`"`) {
			t.Errorf("response does not contain model option %q", model)
		}
	}
	for _, tool := range []string{"webfetch", "websearch"} {
		if !strings.Contains(response.Body.String(), `name="tool" value="`+tool+`"`) {
			t.Errorf("response does not contain tool option %q", tool)
		}
	}
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
	if !strings.Contains(body, `name="tool" value="websearch" form="message-form" checked`) {
		t.Errorf("websearch not checked: %s", body)
	}
	if strings.Contains(body, `name="tool" value="webfetch" form="message-form" checked`) {
		t.Errorf("webfetch unexpectedly checked: %s", body)
	}
}

func TestMessageCompletionHandlerPersistsChatTools(t *testing.T) {
	database := openTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	form := url.Values{
		"message": {"Hello"},
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls)},
		"tool":    {"webfetch", "websearch"},
	}
	request := httptest.NewRequest(http.MethodPost, "/messages/complete?chat=3", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)(response, request)

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
	for _, content := range []string{
		`class="history-action history-edit"`,
		`class="history-action history-cancel"`,
		`class="history-action history-delete"`,
		`hx-put="/chats/8?current=8"`,
		`hx-delete="/chats/8?current=8"`,
		`hx-confirm="Permanently delete Project notes?"`,
		`aria-label="Delete Project notes"`,
		`name="title"`,
		`value="Project notes"`,
	} {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("response does not contain %q", content)
		}
	}
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
	if !strings.Contains(response.Body.String(), "New title") {
		t.Error("response does not contain renamed chat")
	}
	if strings.Contains(response.Body.String(), "Old title") {
		t.Error("response still contains old title")
	}
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
	if strings.Contains(response.Body.String(), "Delete me") {
		t.Error("response still contains deleted chat")
	}
	if !strings.Contains(response.Body.String(), "Keep me") {
		t.Error("response does not contain remaining chat")
	}
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

	if redirect := response.Header().Get("HX-Redirect"); redirect != "/" {
		t.Errorf("HX-Redirect = %q, want /", redirect)
	}
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
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	toolCalls := newToolCallStore()
	messageHandler(database, newTestToolRegistry(t), toolCalls)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	for _, content := range []string{
		"Hello",
		"selected-model",
		`hx-post="/messages/complete?chat=1"`,
		`hx-trigger="load"`,
		`hx-get="/messages/tools?request=`,
		`hx-swap-oob="outerHTML"`,
		`name="request" value="`,
		`name="tool" value="webfetch"`,
	} {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("response does not contain %q", content)
		}
	}
}

func TestMessageToolStatusHandlerRendersToolCallState(t *testing.T) {
	toolCalls := newToolCallStore()
	requestID := newToolCallRequest(t, toolCalls)
	tracker, ok := toolCalls.claim(requestID)
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
	response := httptest.NewRecorder()
	messageToolStatusHandler(toolCalls)(response, request)

	for _, content := range []string{"websearch", "braille spinner", `class="braille-spinner"`, "Running tool:"} {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("running response does not contain %q: %s", content, response.Body.String())
		}
	}

	tracker.observe(call, false)
	response = httptest.NewRecorder()
	messageToolStatusHandler(toolCalls)(response, request)
	if strings.Contains(response.Body.String(), `class="braille-spinner"`) {
		t.Errorf("completed response contains spinner: %s", response.Body.String())
	}
	for _, content := range []string{`class="tool-call-complete"`, "Completed tool:"} {
		if !strings.Contains(response.Body.String(), content) {
			t.Errorf("completed response does not contain %q: %s", content, response.Body.String())
		}
	}
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
	for _, message := range []string{"My name is Cassian.", "What is my name?"} {
		form := url.Values{
			"message": {message},
			"model":   {"selected-model"},
			"request": {newToolCallRequest(t, toolCalls)},
			"tool":    {"webfetch"},
		}
		request := httptest.NewRequest(http.MethodPost, "/messages?chat=1", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()

		handler(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "<strong>response-model</strong>") {
			t.Error("response does not label assistant message with model")
		}
		if strings.Contains(response.Body.String(), "<strong>test-model</strong>") {
			t.Error("response labels assistant message with environment model")
		}
		if strings.Contains(response.Body.String(), "<strong>assistant</strong>") {
			t.Error("response labels assistant message with role")
		}
	}

	firstRequest := <-requests
	if len(firstRequest.Messages) != 2 || firstRequest.Messages[0].Role != "system" || firstRequest.Messages[0].Content == "" || firstRequest.Messages[1].Content != "My name is Cassian." {
		t.Fatalf("first request messages = %#v, want system prompt and first user message", firstRequest.Messages)
	}
	secondRequest := <-requests
	if len(secondRequest.Messages) != 4 {
		t.Fatalf("second request message count = %d, want 4", len(secondRequest.Messages))
	}
	if secondRequest.Messages[0].Role != "system" || secondRequest.Messages[0].Content != firstRequest.Messages[0].Content {
		t.Errorf("second request system message = %#v, want %#v", secondRequest.Messages[0], firstRequest.Messages[0])
	}
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
	form := url.Values{
		"message": {"Question"},
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls)},
	}
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	for _, rendered := range []string{"<strong>Answer</strong>", "<li>one</li>", "<li>two</li>"} {
		if !strings.Contains(response.Body.String(), rendered) {
			t.Errorf("response does not contain rendered Markdown %q: %s", rendered, response.Body.String())
		}
	}

	const want = "**Answer**\n\n- one\n- two"
	var stored string
	if err := database.QueryRow(`SELECT content FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&stored); err != nil {
		t.Fatalf("get stored assistant message: %v", err)
	}
	if stored != want {
		t.Errorf("stored content = %q, want raw Markdown %q", stored, want)
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
	requestID := newToolCallRequest(t, toolCalls)
	form := url.Values{
		"message": {"Use a tool"},
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
	statusResponse := httptest.NewRecorder()
	messageToolStatusHandler(toolCalls)(statusResponse, statusRequest)
	for _, content := range []string{"webfetch", `class="braille-spinner"`} {
		if !strings.Contains(statusResponse.Body.String(), content) {
			t.Errorf("running tool response does not contain %q: %s", content, statusResponse.Body.String())
		}
	}

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
	for _, content := range []string{"webfetch", fetched.URL, "Final answer.", `class="tool-call-complete"`} {
		if !strings.Contains(body, content) {
			t.Errorf("response does not contain %q: %s", content, body)
		}
	}
	if strings.Contains(body, `class="braille-spinner"`) {
		t.Errorf("completed response contains spinner: %s", body)
	}
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

	reloadRequest := httptest.NewRequest(http.MethodGet, "/chat?chat=1", nil)
	reloadResponse := httptest.NewRecorder()
	chatHandler(database)(reloadResponse, reloadRequest)
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("reload status = %d, want %d; body = %q", reloadResponse.Code, http.StatusOK, reloadResponse.Body.String())
	}
	reloaded := reloadResponse.Body.String()
	for _, content := range []string{"webfetch", fetched.URL, "Final answer.", `class="tool-call-complete"`} {
		if !strings.Contains(reloaded, content) {
			t.Errorf("reloaded chat does not contain %q: %s", content, reloaded)
		}
	}
	if strings.Contains(reloaded, "fetched content") {
		t.Errorf("reloaded chat exposes tool result: %s", reloaded)
	}
	if strings.Index(reloaded, "webfetch") > strings.Index(reloaded, "Final answer.") {
		t.Errorf("reloaded tool call does not precede answer: %s", reloaded)
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

func newToolCallRequest(t *testing.T, toolCalls *toolCallStore) string {
	t.Helper()

	requestID, err := toolCalls.create()
	if err != nil {
		t.Fatalf("create tool-call tracker: %v", err)
	}
	return requestID
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
