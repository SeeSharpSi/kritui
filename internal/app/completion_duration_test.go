package app

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
)

func TestMigrateDatabaseAddsCompletionDurationFromVersion41(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	messagesColumn := "    duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),\n"
	settingsColumn := "    show_completion_time INTEGER NOT NULL DEFAULT 0 CHECK (show_completion_time IN (0, 1)),\n"
	if !strings.Contains(schema, messagesColumn) || !strings.Contains(schema, settingsColumn) {
		t.Fatal("schema does not contain completion duration columns")
	}
	previous := strings.Replace(schema, messagesColumn, "", 1)
	previous = strings.Replace(previous, settingsColumn, "", 1)
	previous = strings.Replace(previous, "PRAGMA user_version = 42;", "PRAGMA user_version = 41;", 1)
	if _, err := database.Exec(previous); err != nil {
		t.Fatalf("initialize version 41 database: %v", err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	var version, messagesColumnCount, settingsColumnCount int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'duration_ms'`).Scan(&messagesColumnCount); err != nil {
		t.Fatalf("inspect messages duration column: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('settings') WHERE name = 'show_completion_time'`).Scan(&settingsColumnCount); err != nil {
		t.Fatalf("inspect settings completion time column: %v", err)
	}
	if messagesColumnCount != 1 || settingsColumnCount != 1 {
		t.Fatalf("duration columns = %d messages, %d settings; want 1 and 1", messagesColumnCount, settingsColumnCount)
	}
	enabled, err := kritui_db.GetShowCompletionTime(context.Background(), database)
	if err != nil || enabled {
		t.Fatalf("show completion time = %v, %v; want false and nil", enabled, err)
	}
}

func TestSettingsToggleShowCompletionTime(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	handler := settingsHandler(database, newTestToolRegistry(t))

	get := httptest.NewRecorder()
	handler(get, httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", get.Code)
	}
	requireContains(t, get.Body.String(), `id="show-completion-time"`, `name="show_completion_time" value="1"`)

	enable := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":                {"saved-model"},
		"max_tool_rounds":      {"16"},
		"show_completion_time": {"1"},
	})
	if enable.Code != http.StatusOK {
		t.Fatalf("enable status = %d; body = %q", enable.Code, enable.Body.String())
	}
	if enabled, err := kritui_db.GetShowCompletionTime(context.Background(), database); err != nil || !enabled {
		t.Fatalf("show completion time after enable = %v, %v", enabled, err)
	}

	disable := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
	})
	if disable.Code != http.StatusOK {
		t.Fatalf("disable status = %d; body = %q", disable.Code, disable.Body.String())
	}
	if enabled, err := kritui_db.GetShowCompletionTime(context.Background(), database); err != nil || enabled {
		t.Fatalf("show completion time after disable = %v, %v", enabled, err)
	}
}

func TestMessageCompletionStoresTurnDuration(t *testing.T) {
	database := openTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"selected-model","choices":[{"message":{"role":"assistant","content":"Timed answer"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL)
	t.Setenv("LLM_MODEL", "selected-model")

	toolCalls := newToolCallStore()
	handler := messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)
	insertAcceptedUser(t, database, 1, "question")
	response := completeForm(t, handler, toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"selected-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Timed answer") {
		t.Fatalf("completion body = %q", response.Body.String())
	}
	var duration sql.NullInt64
	if err := database.QueryRow(`SELECT duration_ms FROM messages WHERE chat_id = 1 AND role = 'assistant' ORDER BY position DESC LIMIT 1`).Scan(&duration); err != nil {
		t.Fatalf("read completion duration: %v", err)
	}
	if !duration.Valid || duration.Int64 < 20 {
		t.Fatalf("stored duration = %#v, want at least 20ms", duration)
	}
}
