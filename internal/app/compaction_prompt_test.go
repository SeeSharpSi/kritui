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

func settingsCompactionPromptForm(custom string) url.Values {
	return url.Values{
		"model":                  {"saved-model"},
		"max_tool_rounds":        {"16"},
		"compaction_prompt_form": {"1"},
		"compaction_prompt":      {custom},
	}
}

func TestSettingsHandlerRendersEffectiveCompactionPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	request := httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil)
	response := httptest.NewRecorder()

	settingsHandler(database, newTestToolRegistry(t))(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), llm.DefaultCompactionPrompt())
}

func TestSettingsHandlerStoresCompactionPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Custom compaction prompt.\nSecond line."

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsCompactionPromptForm(custom))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	prompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Fatalf("stored compaction prompt = %q, want %q", prompt, custom)
	}
	requireContains(t, response.Body.String(), "Custom compaction prompt.", "Second line.")
}

func TestSettingsHandlerOmittedCompactionPromptIsPreserved(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Preserved without resubmission."
	first := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsCompactionPromptForm(custom))
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
	prompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != custom {
		t.Fatalf("stored compaction prompt = %q, want preserved %q", prompt, custom)
	}
}

func TestSettingsHandlerBlankCompactionPromptResetsToDefault(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:            "saved-model",
		MaxToolRounds:    16,
		CompactionPrompt: strPtr("Previous custom prompt."),
	}); err != nil {
		t.Fatalf("seed compaction prompt: %v", err)
	}

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", settingsCompactionPromptForm(" \n\t "))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	prompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != llm.DefaultCompactionPrompt() {
		t.Fatalf("stored compaction prompt = %q, want default reset", prompt)
	}
	requireContains(t, response.Body.String(), llm.DefaultCompactionPrompt())
}

func TestSettingsHandlerInvalidSettingsRerenderUnsavedCompactionPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:            "saved-model",
		MaxToolRounds:    16,
		CompactionPrompt: strPtr("Saved prompt."),
		SystemPrompt:     strPtr("Saved system prompt."),
	}); err != nil {
		t.Fatalf("seed compaction prompt: %v", err)
	}

	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":                  {"invalid-settings-model"},
		"max_tool_rounds":        {"0"},
		"compaction_prompt_form": {"1"},
		"compaction_prompt":      {"Unsaved custom prompt."},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Unsaved custom prompt.", "Saved system prompt.")
	prompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() error: %v", err)
	}
	if prompt != "Saved prompt." {
		t.Fatalf("stored compaction prompt after invalid save = %q, want saved prompt", prompt)
	}
	systemPrompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetSystemPrompt() error: %v", err)
	}
	if systemPrompt != "Saved system prompt." {
		t.Fatalf("stored system prompt after invalid save = %q, want saved system prompt", systemPrompt)
	}
}

func TestSettingsHandlerPromptValidationPreservesOtherUnsavedPrompt(t *testing.T) {
	tests := []struct {
		name             string
		systemPrompt     string
		compactionPrompt string
		wantError        string
		wantRendered     string
	}{
		{
			name:             "invalid system prompt preserves compaction",
			systemPrompt:     strings.Repeat("s", kritui_db.MaxSystemPromptBytes+1),
			compactionPrompt: "Unsaved compaction prompt.",
			wantError:        "System prompt is invalid",
			wantRendered:     "Unsaved compaction prompt.",
		},
		{
			name:             "invalid compaction prompt preserves system",
			systemPrompt:     "Unsaved system prompt.",
			compactionPrompt: strings.Repeat("c", kritui_db.MaxCompactionPromptBytes+1),
			wantError:        "Compaction prompt is invalid",
			wantRendered:     "Unsaved system prompt.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDatabase(t)
			seedCurrentChat(t, database, 8)
			if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
				Model:            "saved-model",
				MaxToolRounds:    16,
				SystemPrompt:     strPtr("Saved system prompt."),
				CompactionPrompt: strPtr("Saved compaction prompt."),
			}); err != nil {
				t.Fatalf("seed prompts: %v", err)
			}

			response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
				"model":                  {"saved-model"},
				"max_tool_rounds":        {"16"},
				"system_prompt_form":     {"1"},
				"system_prompt":          {test.systemPrompt},
				"compaction_prompt_form": {"1"},
				"compaction_prompt":      {test.compactionPrompt},
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
			}
			requireContains(t, response.Body.String(), test.wantError, test.wantRendered)
			systemPrompt, err := kritui_db.GetSystemPrompt(context.Background(), database)
			if err != nil {
				t.Fatalf("GetSystemPrompt() error: %v", err)
			}
			if systemPrompt != "Saved system prompt." {
				t.Errorf("stored system prompt = %q, want saved prompt", systemPrompt)
			}
			compactionPrompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
			if err != nil {
				t.Fatalf("GetCompactionPrompt() error: %v", err)
			}
			if compactionPrompt != "Saved compaction prompt." {
				t.Errorf("stored compaction prompt = %q, want saved compaction prompt", compactionPrompt)
			}
		})
	}
}

func TestHomeHandlerRendersStoredCompactionPrompt(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	custom := "Home hidden compaction prompt."
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:            "saved-model",
		MaxToolRounds:    16,
		CompactionPrompt: strPtr(custom),
	}); err != nil {
		t.Fatalf("seed compaction prompt: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?chat=8", nil)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), custom)
}

func TestMigrateDatabaseAddsCompactionPromptFromVersion33(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	column := "    compaction_prompt TEXT,\n"
	if !strings.Contains(schema, column) {
		t.Fatal("schema does not contain compaction_prompt column")
	}
	previous := strings.Replace(schema, column, "", 1)
	previous = strings.Replace(previous, "PRAGMA user_version = 34;", "PRAGMA user_version = 33;", 1)
	if _, err := database.Exec(previous); err != nil {
		t.Fatalf("initialize version 33 database: %v", err)
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
	prompt, err := kritui_db.GetCompactionPrompt(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionPrompt() after migration error: %v", err)
	}
	if prompt != llm.DefaultCompactionPrompt() {
		t.Fatalf("migrated compaction prompt = %q, want default", prompt)
	}
}

func TestMessageCompletionSummaryUsesStoredCompactionPrompt(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const (
		chatID       = 84
		mainModel    = "main-model"
		summaryModel = "summary-model"
	)
	custom := "\n  Summarize with the account compaction prompt.  \n"
	seedToolElisionHistory(t, database, chatID)
	insertAcceptedUser(t, database, chatID, "New question")
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: summaryModel}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model:             mainModel,
		MaxToolRounds:     16,
		ToolResultElision: &config,
		CompactionPrompt:  &custom,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, mainModel, llm.EndpointChatCompletions); err != nil {
		t.Fatalf("set main model endpoint preference: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, summaryModel, llm.EndpointMessages); err != nil {
		t.Fatalf("set summary model endpoint preference: %v", err)
	}

	summaryRequests := make(chan toolElisionMessagesRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/messages":
			var request toolElisionMessagesRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode summary request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			summaryRequests <- request
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"role":"assistant","model":"summary-model","content":[{"type":"text","text":"Earlier result: retained key value."}],"stop_reason":"end_turn"}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(toolElisionFinalResponse))
		default:
			t.Errorf("unexpected provider path %q", r.URL.Path)
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "local-test-key")
	t.Setenv("LLM_MODEL", mainModel)
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newToolElisionLookupRegistry(t), toolCalls, nil), toolCalls,
		"/messages/complete?chat=84", url.Values{
			"model":   {mainModel},
			"request": {newToolCallRequest(t, toolCalls, chatID)},
			"tool":    {"lookup"},
		})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Fresh answer")

	summaryRequest := receiveToolElisionValue(t, summaryRequests, "summary provider request")
	if summaryRequest.System != custom {
		t.Errorf("summary system prompt = %q, want exact custom prompt %q", summaryRequest.System, custom)
	}
	if len(summaryRequest.Tools) != 0 || len(summaryRequest.Messages) != 1 {
		t.Errorf("summary request tools/messages = %d/%d, want tool-free two-message request", len(summaryRequest.Tools), len(summaryRequest.Messages))
	}
}
