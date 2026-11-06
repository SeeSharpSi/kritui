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
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

const reasoningModelMetadata = `{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}`

func reasoningTestCatalog(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_ENDPOINT", "https://reasoning.example/v1/responses")
	useModelsDevService(t, newCatalogService(t, catalogFixture("https://reasoning.example/v1", "test", reasoningModelMetadata)))
}

func TestThinkingPickerAndValidation(t *testing.T) {
	reasoningTestCatalog(t)
	database := openTestDatabase(t)
	for _, test := range []struct{ scope, modelField, effortField string }{
		{"chat", "model", "reasoning_effort"},
		{"default", "model", "default_reasoning_effort"},
		{"title", "title_generation_model", "title_reasoning_effort"},
	} {
		for _, model := range []string{"test", "unknown"} {
			query := url.Values{"scope": {test.scope}, test.modelField: {model}, test.effortField: {"high"}}
			response := httptest.NewRecorder()
			thinkingPickerHandler(database)(response, httptest.NewRequest("GET", "/thinking?"+query.Encode(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("picker status %d", response.Code)
			}
			requireContains(t, response.Body.String(), `id="`+test.scope+`-thinking"`, `name="`+test.effortField+`"`, `>Default</span>`)
			if model == "test" {
				requireContains(t, response.Body.String(), `value="high"`, `Thinking: High`)
			} else {
				requireContains(t, response.Body.String(), `Thinking: Default`, `aria-disabled="true"`)
			}
		}
	}
	if err := validateModelReasoning("https://reasoning.example/v1/responses", "test", "max"); err == nil {
		t.Fatal("accepted unsupported effort")
	}
	if err := validateModelReasoning("https://reasoning.example/v1/responses", "unknown", ""); err != nil {
		t.Fatal(err)
	}
	// Reasoning-only catalog entries must not be discarded for lacking modalities.
	if len(modelDetailsForEndpoint("https://reasoning.example/v1/responses", []string{"test"})["test"].ReasoningEfforts) != 3 {
		t.Fatal("reasoning-only model details missing")
	}
}

func TestSettingsThinkingSelections(t *testing.T) {
	reasoningTestCatalog(t)
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	values := url.Values{"model": {"test"}, "max_tool_rounds": {"16"}, "title_generation_model": {"test"}, "default_reasoning_effort": {"high"}, "title_reasoning_effort": {"low"}}
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", values)
	if response.Code != http.StatusOK {
		t.Fatalf("save status %d: %s", response.Code, response.Body.String())
	}
	ctx := context.Background()
	if got, err := kritui_db.GetDefaultReasoningEffort(ctx, database); err != nil || got != "high" {
		t.Fatalf("default effort = %q, %v", got, err)
	}
	if got, err := kritui_db.GetTitleGenerationSettings(ctx, database); err != nil || got.ReasoningEffort != "low" {
		t.Fatalf("title effort = %#v, %v", got, err)
	}
	values.Set("default_reasoning_effort", "max")
	response = postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", values)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported effort status %d", response.Code)
	}
	values.Set("default_reasoning_effort", "")
	values.Del("title_reasoning_effort")
	response = postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", values)
	if response.Code != http.StatusOK {
		t.Fatalf("Default status %d", response.Code)
	}
	if got, _ := kritui_db.GetDefaultReasoningEffort(ctx, database); got != "" {
		t.Fatalf("Default persisted as %q", got)
	}
	if got, _ := kritui_db.GetTitleGenerationSettings(ctx, database); got.ReasoningEffort != "low" {
		t.Fatal("omitted title effort was overwritten")
	}
}

func TestMessageThinkingDefaultAndCapturedRetry(t *testing.T) {
	reasoningTestCatalog(t)
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	if _, err := database.Exec(`UPDATE settings SET default_reasoning_effort = 'high' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	registry := newTestToolRegistry(t)
	for _, test := range []struct {
		path   string
		values url.Values
		want   string
		valid  bool
	}{
		{"/messages", url.Values{"model": {"test"}}, "high", true},
		{"/messages", url.Values{"model": {"test"}, "reasoning_effort": {""}}, "", true},
		{"/messages", url.Values{"model": {"test"}, "reasoning_effort": {"max"}}, "", false},
	} {
		r := httptest.NewRequest("POST", test.path, strings.NewReader(test.values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = r.ParseForm()
		got, ok := parseMessageOptions(r, database, registry, "8", 8, func(int, string) {})
		if ok != test.valid || ok && got.reasoningEffort != test.want {
			t.Fatalf("options = %#v, %v; want effort %q, valid %v", got, ok, test.want, test.valid)
		}
	}
	state := kritui_db.CompletionState{Pending: true, Model: "test", ReasoningEffort: "low"}
	if err := kritui_db.SetCompletionState(context.Background(), database, 8, state); err != nil {
		t.Fatal(err)
	}
	// Captured work must survive a catalog outage and changed account defaults.
	useModelsDevService(t, nil)
	for _, path := range []string{"/messages/retry", "/messages/complete"} {
		r := httptest.NewRequest("POST", path, strings.NewReader("model=other&reasoning_effort=high"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = r.ParseForm()
		got, ok := parseMessageOptions(r, database, registry, "8", 8, func(int, string) {})
		if !ok || got.model != "test" || got.reasoningEffort != "low" {
			t.Fatalf("captured options = %#v, %v", got, ok)
		}
	}
}

func TestReasoningMigrationFromVersion36(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	previous := strings.Replace(schema, "PRAGMA user_version = 37;", "PRAGMA user_version = 36;", 1)
	for _, column := range []string{"    default_reasoning_effort TEXT NOT NULL DEFAULT '',\n", "    title_reasoning_effort TEXT NOT NULL DEFAULT '',\n", "    reasoning_effort TEXT,\n", "    reasoning_effort TEXT NOT NULL DEFAULT '',\n"} {
		previous = strings.ReplaceAll(previous, column, "")
	}
	if strings.Contains(previous, "reasoning_effort") {
		t.Fatal("old schema fixture still has effort columns")
	}
	if _, err := database.Exec(previous); err != nil {
		t.Fatal(err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatal(err)
	}
	if got, err := kritui_db.GetDefaultReasoningEffort(context.Background(), database); err != nil || got != "" {
		t.Fatalf("migrated default = %q, %v", got, err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatal(err)
	}
}

func TestTitleGenerationUsesIndependentThinking(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	seedCurrentChat(t, database, 1)
	if _, err := persistUserMessage(ctx, database, 1, "A tiny title test", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	high, low := "high", "low"
	if err := kritui_db.SaveSettings(ctx, database, kritui_db.SettingsUpdate{
		Model: "chat", MaxToolRounds: 16, ReasoningEffort: &high, TitleReasoningEffort: &low,
		TitleGeneration: &kritui_db.TitleGenerationSettingsUpdate{Enabled: true, Model: "title"},
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "title" || payload.Effort != "low" {
			t.Errorf("title request = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Tiny test"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/chat/completions")
	if changed, err := generateChatTitle(ctx, database, 1); err != nil || !changed {
		t.Fatalf("title generation = %v, %v", changed, err)
	}
}

func TestEditedMessageCapturesThinking(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	seedCurrentChat(t, database, 1)
	message, err := persistUserMessage(ctx, database, 1, "Original", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := kritui_db.CompletionState{Pending: true, Model: "edited-model", ReasoningEffort: "high"}
	if err := editUserMessage(ctx, database, 1, message.id, "Edited", nil, nil, state); err != nil {
		t.Fatal(err)
	}
	got, err := kritui_db.GetCompletionState(ctx, database, 1)
	if err != nil || !got.Pending || got.Model != state.Model || got.ReasoningEffort != state.ReasoningEffort {
		t.Fatalf("edited completion state = %#v, %v", got, err)
	}
	if got, err := kritui_db.GetChatReasoningEffort(ctx, database, 1); err != nil || got != "high" {
		t.Fatalf("edited chat effort = %q, %v", got, err)
	}
}

func TestQueuedCompletionUsesCapturedThinking(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	seedCurrentChat(t, database, 1)
	if _, err := persistUserMessage(ctx, database, 1, "First", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := kritui_db.EnqueuePrompt(ctx, database, 1, kritui_db.InboxItem{
		SubmissionID: "queued", Delivery: "queue", Model: "queued-model", ReasoningEffort: "low",
		Message: llm.Message{Role: "user", Content: "Next"},
	}); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload.Model + ":" + payload.Effort
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/chat/completions")
	registry := newTestToolRegistry(t)
	request, err := requestForInbox(registry, 1, "first-model", nil, "high")
	if err != nil {
		t.Fatal(err)
	}
	store := newToolCallStore()
	id, err := store.create(1, request.model, request.toolNames)
	if err != nil {
		t.Fatal(err)
	}
	defer store.delete(id)
	tracker, ok := store.claim(id, 1, request.model, request.toolNames)
	if !ok {
		t.Fatal("claim failed")
	}
	completionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	runMessageCompletion(completionCtx, database, registry, request, id, tracker, store, nil, "")
	for _, want := range []string{"first-model:high", "queued-model:low"} {
		select {
		case got := <-requests:
			if got != want {
				t.Fatalf("request = %q, want %q", got, want)
			}
		default:
			t.Fatalf("missing request %q", want)
		}
	}
}
