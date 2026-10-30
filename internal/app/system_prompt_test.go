package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func settingsSystemPromptForm(custom string) url.Values {
	return url.Values{
		"model":              {"saved-model"},
		"max_tool_rounds":    {"16"},
		"system_prompt_form": {"1"},
		"system_prompt":      {custom},
	}
}

func TestSettingsHandlerRendersEffectiveSystemPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil)
	response := httptest.NewRecorder()

	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), llm.DefaultSystemPrompt())
}

func TestSettingsHandlerStoresSystemPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Custom system prompt.\nSecond line."

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsSystemPromptForm(custom))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	prompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Fatalf("stored system prompt = %q, want %q", prompt, custom)
	}
	requireContains(t, response.Body.String(), "Custom system prompt.", "Second line.")
}

func TestSettingsHandlerOmittedSystemPromptIsPreserved(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Preserved without resubmission."
	first := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsSystemPromptForm(custom))
	if first.Code != http.StatusOK {
		t.Fatalf("first save status = %d; body = %q", first.Code, first.Body.String())
	}

	second := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"other-model"},
		"max_tool_rounds": {"8"},
	})
	if second.Code != http.StatusOK {
		t.Fatalf("second save status = %d; body = %q", second.Code, second.Body.String())
	}
	prompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Fatalf("stored system prompt = %q, want preserved %q", prompt, custom)
	}
}

func TestSettingsHandlerBlankSystemPromptResetsToDefault(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 16,
		SystemPrompt:  strPtr("Previous custom prompt."),
	}); err != nil {
		t.Fatalf("seed system prompt: %v", err)
	}

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsSystemPromptForm(" \n\t "))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	prompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != llm.DefaultSystemPrompt() {
		t.Fatalf("stored system prompt = %q, want default reset", prompt)
	}
	requireContains(t, response.Body.String(), llm.DefaultSystemPrompt())
}

func TestSettingsHandlerInvalidSettingsRerenderUnsavedSystemPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 16,
		SystemPrompt:  strPtr("Saved prompt."),
	}); err != nil {
		t.Fatalf("seed system prompt: %v", err)
	}

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":              {"invalid-settings-model"},
		"max_tool_rounds":    {"0"},
		"system_prompt_form": {"1"},
		"system_prompt":      {"Unsaved custom prompt."},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Unsaved custom prompt.")
	prompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if prompt != "Saved prompt." {
		t.Fatalf("stored system prompt after invalid save = %q, want saved prompt", prompt)
	}
	model, err := kritui_db.GetDefaultModel(context.Background(), database, "fallback")
	if err != nil {
		t.Fatalf("GetDefaultModel() error: %v", err)
	}
	if model != "saved-model" {
		t.Fatalf("default model after invalid save = %q, want saved-model", model)
	}
}

func TestHomeHandlerRendersStoredSystemPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Home hidden system prompt."
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 16,
		SystemPrompt:  strPtr(custom),
	}); err != nil {
		t.Fatalf("seed system prompt: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), custom)
}

func TestMigrateDatabaseAddsSystemPromptFromVersion32(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	column := "    system_prompt TEXT,\n"
	if !strings.Contains(schema, column) {
		t.Fatal("schema does not contain system_prompt column")
	}
	previous := strings.Replace(schema, column, "", 1)
	previous = strings.Replace(previous, "PRAGMA user_version = 33;", "PRAGMA user_version = 32;", 1)
	if _, err := database.Exec(previous); err != nil {
		t.Fatalf("initialize version 32 database: %v", err)
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
	prompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() after migration error: %v", err)
	}
	if prompt != llm.DefaultSystemPrompt() {
		t.Fatalf("migrated system prompt = %q, want default", prompt)
	}
}

func TestMessageCompletionUsesStoredSystemPromptWithRuntimeContext(t *testing.T) {
	database := openTestDatabase(t)
	custom := "\n  Account-specific system base.  \n\nSecond paragraph.\n"
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:         "saved-model",
		MaxToolRounds: 16,
		SystemPrompt:  strPtr(custom),
	}); err != nil {
		t.Fatalf("seed system prompt: %v", err)
	}

	type providerRequest struct {
		Messages []llm.Message `json:"messages"`
	}
	captured := make(chan providerRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		captured <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"response-model",
			"choices":[{"message":{"role":"assistant","content":"Answer"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_ENDPOINT", server.URL)

	toolCalls := newToolCallStore()
	insertAcceptedUser(t, database, 1, "Question")
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls,
		"/messages/complete?chat=1", url.Values{
			"model":           {"selected-model"},
			"request":         {newToolCallRequest(t, toolCalls, 1)},
			"client_timezone": {"America/New_York"},
		})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}

	request := <-captured
	if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
		t.Fatalf("provider messages = %#v, want system lead", request.Messages)
	}
	system := request.Messages[0].Content
	if !strings.HasPrefix(system, custom+"\n\n") {
		t.Errorf("system prompt = %q, want raw custom prefix %q", system, custom+"\n\n")
	}
	if strings.HasPrefix(system, strings.TrimSpace(custom)) {
		t.Errorf("system prompt = %q, want exact custom whitespace instead of trimmed", system)
	}
	if !strings.Contains(system, "When the user attaches PDFs, cite pages with Markdown links") {
		t.Errorf("system prompt = %q, want invariant PDF guidance", system)
	}
	if !strings.Contains(system, "Current UTC datetime:") ||
		!strings.Contains(system, "Client timezone: America/New_York") {
		t.Errorf("system prompt = %q, want runtime context", system)
	}
	if strings.Contains(system, llm.DefaultSystemPrompt()) {
		t.Errorf("system prompt = %q, want custom base instead of built-in", system)
	}
}

func strPtr(value string) *string {
	return &value
}
