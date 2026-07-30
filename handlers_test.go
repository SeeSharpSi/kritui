package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
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

func TestHomeHandlerRendersStoredMessages(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (8);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(8, 0, 'user', 'Earlier question'),
			(8, 1, 'assistant', 'Earlier answer');
	`); err != nil {
		t.Fatalf("insert chat history: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database)(response, request)

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
}

func TestHomeHandlerRendersWelcomeForEmptyChat(t *testing.T) {
	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()

	homeHandler(database)(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "What would you like to discuss?") {
		t.Error("response does not contain welcome prompt")
	}
}

func TestMessageHandlerIncludesEarlierMessages(t *testing.T) {
	database := openTestDatabase(t)
	requests := make(chan []llm.Message, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"Remembered."},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	handler := messageHandler(database)
	for _, message := range []string{"My name is Cassian.", "What is my name?"} {
		form := url.Values{"message": {message}}
		request := httptest.NewRequest(http.MethodPost, "/messages?chat=1", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()

		handler(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
	}

	firstRequest := <-requests
	if len(firstRequest) != 1 || firstRequest[0].Content != "My name is Cassian." {
		t.Fatalf("first request messages = %#v, want first user message", firstRequest)
	}
	secondRequest := <-requests
	if len(secondRequest) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(secondRequest))
	}
	want := []llm.Message{
		{Role: "user", Content: "My name is Cassian."},
		{Role: "assistant", Content: "Remembered."},
		{Role: "user", Content: "What is my name?"},
	}
	for index := range want {
		if secondRequest[index].Role != want[index].Role || secondRequest[index].Content != want[index].Content {
			t.Errorf("second request message %d = %#v, want %#v", index, secondRequest[index], want[index])
		}
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
