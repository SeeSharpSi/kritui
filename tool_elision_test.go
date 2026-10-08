package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/tools"
)

func TestSettingsHandlerToolResultElisionRoundTripsAndHomeReload(t *testing.T) {
	setToolElisionSettingsEnvironment(t)
	database := openTestDatabase(t)
	handler := settingsHandler(database, newTestToolRegistry(t))

	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial settings status = %d, want %d; body = %q", initial.Code, http.StatusOK, initial.Body.String())
	}
	if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
		t.Fatalf("get initial elision config: %v", err)
	} else if want := (llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone}); got != want {
		t.Fatalf("initial elision config = %#v, want %#v", got, want)
	}
	requireToolElisionModeSelected(t, initial.Body.String(), llm.ToolResultElisionNone)

	tests := []struct {
		name   string
		config llm.ToolResultElisionConfig
	}{
		{name: "none", config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone}},
		{name: "current turn", config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn}},
		{name: "last user turns", config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 7}},
		{name: "token budget", config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 384}},
		{name: "summarize", config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: "summary-model"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			form := toolElisionSettingsForm(test.config)
			response := postForm(t, handler, "/settings?chat=8", form)
			if response.Code != http.StatusOK {
				t.Fatalf("settings status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
			}
			got, err := kritui_db.GetToolResultElision(context.Background(), database)
			if err != nil {
				t.Fatalf("get saved elision config: %v", err)
			}
			if got != test.config {
				t.Errorf("saved elision config = %#v, want %#v", got, test.config)
			}
			requireToolElisionModeSelected(t, response.Body.String(), test.config.Mode)
			switch test.config.Mode {
			case llm.ToolResultElisionLastUserTurns:
				requireContains(t, response.Body.String(), `id="tool-result-elision-user-turns"`, `value="7"`)
			case llm.ToolResultElisionBudget:
				requireContains(t, response.Body.String(), `id="tool-result-elision-token-budget"`, `value="384"`)
			case llm.ToolResultElisionSummarize:
				requireContains(t, response.Body.String(), `id="tool-result-elision-summary-model"`, `value="summary-model"`)
			}
		})
	}

	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (8, 'settings reload')`); err != nil {
		t.Fatalf("seed home chat: %v", err)
	}
	page := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(
		page,
		httptest.NewRequest(http.MethodGet, "/?chat=8", nil),
	)
	if page.Code != http.StatusOK {
		t.Fatalf("home status = %d, want %d; body = %q", page.Code, http.StatusOK, page.Body.String())
	}
	requireToolElisionModeSelected(t, page.Body.String(), llm.ToolResultElisionSummarize)
	requireContains(t, page.Body.String(), `id="tool-result-elision-summary-model"`, `value="summary-model"`)
}

func TestSettingsHandlerRejectsInvalidActiveToolResultElisionWithoutSaving(t *testing.T) {
	setToolElisionSettingsEnvironment(t)
	tests := []struct {
		name   string
		mode   string
		field  string
		values []string
	}{
		{name: "user turns missing", mode: string(llm.ToolResultElisionLastUserTurns), field: "tool_result_elision_user_turns", values: []string{""}},
		{name: "user turns noninteger", mode: string(llm.ToolResultElisionLastUserTurns), field: "tool_result_elision_user_turns", values: []string{"many"}},
		{name: "user turns below range", mode: string(llm.ToolResultElisionLastUserTurns), field: "tool_result_elision_user_turns", values: []string{"0"}},
		{name: "user turns above range", mode: string(llm.ToolResultElisionLastUserTurns), field: "tool_result_elision_user_turns", values: []string{"1001"}},
		{name: "budget missing", mode: string(llm.ToolResultElisionBudget), field: "tool_result_elision_token_budget", values: []string{""}},
		{name: "budget noninteger", mode: string(llm.ToolResultElisionBudget), field: "tool_result_elision_token_budget", values: []string{"many"}},
		{name: "budget below range", mode: string(llm.ToolResultElisionBudget), field: "tool_result_elision_token_budget", values: []string{"0"}},
		{name: "budget above range", mode: string(llm.ToolResultElisionBudget), field: "tool_result_elision_token_budget", values: []string{"10000001"}},
		{name: "summary model missing", mode: string(llm.ToolResultElisionSummarize), field: "tool_result_elision_summary_model", values: []string{""}},
		{name: "summary model oversized", mode: string(llm.ToolResultElisionSummarize), field: "tool_result_elision_summary_model", values: []string{strings.Repeat("m", 513)}},
		{name: "unknown mode", mode: "discard", values: []string{""}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDatabase(t)
			baseline := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 4}
			if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
				Model:             "old-model",
				MaxToolRounds:     3,
				DefaultTools:      []string{"websearch"},
				ToolResultElision: &baseline,
				Theme:             "nord",
			}); err != nil {
				t.Fatalf("seed settings: %v", err)
			}

			form := toolElisionSettingsForm(llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionMode(test.mode)})
			form.Set("model", "new-model")
			form.Set("max_tool_rounds", "9")
			form.Set("theme", "1975")
			form.Set("default_tool", "webfetch")
			if len(test.values) > 0 && test.values[0] != "" {
				form.Set(test.field, test.values[0])
			} else if test.field != "" {
				form.Del(test.field)
			}

			response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", form)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
			}
			requireContains(t, response.Body.String(), "Tool-result elision settings are invalid")

			if got, err := kritui_db.GetDefaultModel(context.Background(), database, ""); err != nil {
				t.Fatalf("get default model after rejection: %v", err)
			} else if got != "old-model" {
				t.Errorf("default model after rejected save = %q, want old-model", got)
			}
			if got, err := kritui_db.GetMaxToolRounds(context.Background(), database, 1); err != nil {
				t.Fatalf("get tool rounds after rejection: %v", err)
			} else if got != 3 {
				t.Errorf("max tool rounds after rejected save = %d, want 3", got)
			}
			if got, err := kritui_db.GetDefaultEnabledTools(context.Background(), database, nil); err != nil {
				t.Fatalf("get default tools after rejection: %v", err)
			} else if !slices.Equal(got, []string{"websearch"}) {
				t.Errorf("default tools after rejected save = %v, want [websearch]", got)
			}
			if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
				t.Fatalf("get elision config after rejection: %v", err)
			} else if got != baseline {
				t.Errorf("elision config after rejected save = %#v, want %#v", got, baseline)
			}
			if got, err := kritui_db.GetTheme(context.Background(), database); err != nil {
				t.Fatalf("get theme after rejection: %v", err)
			} else if got != "nord" {
				t.Errorf("theme after rejected save = %q, want nord", got)
			}
		})
	}
}

func TestSettingsHandlerIgnoresInactiveToolResultElisionValues(t *testing.T) {
	setToolElisionSettingsEnvironment(t)
	database := openTestDatabase(t)
	form := toolElisionSettingsForm(llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn})
	form.Set("tool_result_elision_user_turns", "not-a-number")
	form.Set("tool_result_elision_token_budget", "999999999")
	form.Set("tool_result_elision_summary_model", strings.Repeat("x", 600))
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", form)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
		t.Fatalf("get elision config: %v", err)
	} else if want := (llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn}); got != want {
		t.Errorf("elision config with inactive garbage = %#v, want %#v", got, want)
	}
}

func TestSettingsHandlerOmittingToolResultElisionPreservesStoredConfig(t *testing.T) {
	setToolElisionSettingsEnvironment(t)
	database := openTestDatabase(t)
	want := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 2048}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: "old-model", MaxToolRounds: 7, ToolResultElision: &want,
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
		"model":           {"new-model"},
		"max_tool_rounds": {"11"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
		t.Fatalf("get elision config: %v", err)
	} else if got != want {
		t.Errorf("elision config after omitted form = %#v, want %#v", got, want)
	}
}

func TestSettingsHandlerAppendActionsPreserveUnsavedToolResultElision(t *testing.T) {
	setToolElisionSettingsEnvironment(t)
	database := openTestDatabase(t)
	initial, err := kritui_db.GetToolResultElision(context.Background(), database)
	if err != nil {
		t.Fatalf("get initial elision config: %v", err)
	}

	for _, action := range []struct {
		name string
		form func() url.Values
	}{
		{
			name: "add append",
			form: func() url.Values {
				form := toolElisionSettingsForm(llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 640})
				form.Set("append_form", "1")
				form.Set("append_id", "pending")
				form.Set("append_name_pending", "Pending")
				form.Set("append_text_pending", "Unsaved append text")
				form.Set("append_action", "add")
				return form
			},
		},
		{
			name: "remove append",
			form: func() url.Values {
				form := toolElisionSettingsForm(llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 640})
				form.Set("append_form", "1")
				form.Set("append_id", "pending")
				form.Set("append_name_pending", "Pending")
				form.Set("append_text_pending", "Unsaved append text")
				form.Set("remove_append", "pending")
				return form
			},
		},
	} {
		t.Run(action.name, func(t *testing.T) {
			response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", action.form())
			if response.Code != http.StatusOK {
				t.Fatalf("action status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
			}
			requireToolElisionModeSelected(t, response.Body.String(), llm.ToolResultElisionBudget)
			requireContains(t, response.Body.String(), `id="tool-result-elision-token-budget"`, `value="640"`)
			if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
				t.Fatalf("get stored elision config: %v", err)
			} else if got != initial {
				t.Errorf("append action persisted unsaved elision config: got %#v, want %#v", got, initial)
			}
		})
	}
}

func TestMessageCompletionElidesOlderToolResultsOnlyInProviderRequests(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const chatID = 81
	oldResultID, oldResult := seedToolElisionHistory(t, database, chatID)
	insertAcceptedUser(t, database, chatID, "New question")
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: "selected-model", MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}

	requests := make(chan toolElisionChatRequest, 8)
	var mainRequestNumber atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request toolElisionChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode main provider request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		if mainRequestNumber.Add(1) == 1 {
			_, _ = w.Write([]byte(toolElisionToolCallResponse))
			return
		}
		_, _ = w.Write([]byte(toolElisionFinalResponse))
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "local-test-key")
	t.Setenv("LLM_MODEL", "selected-model")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newToolElisionLookupRegistry(t), toolCalls, nil), toolCalls,
		"/messages/complete?chat=81", url.Values{
			"model":   {"selected-model"},
			"request": {newToolCallRequest(t, toolCalls, chatID)},
			"tool":    {"lookup"},
		})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Fresh answer")

	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		request := receiveToolElisionValue(t, requests, "main provider request")
		if request.Model != "selected-model" {
			t.Errorf("provider request model = %q, want selected-model", request.Model)
		}
		old := toolElisionToolResult(t, request.Messages, "old-call")
		if old.Content != "[Tool result elided]" {
			t.Errorf("old provider tool result = %q, want elision marker", old.Content)
		}
		if !toolElisionAssistantHasCall(request.Messages, "old-call") {
			t.Errorf("old assistant tool-call ID was not preserved in provider request %d", requestIndex+1)
		}
		if requestIndex == 1 {
			fresh := toolElisionToolResult(t, request.Messages, "call-new")
			if fresh.Content != "found value" {
				t.Errorf("fresh provider tool result = %q, want original result", fresh.Content)
			}
		}
	}
	if got := mainRequestNumber.Load(); got != 2 {
		t.Errorf("main provider request count = %d, want 2", got)
	}

	stored, err := kritui_db.GetMessages(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get persisted messages: %v", err)
	}
	old := toolElisionToolResult(t, stored, "old-call")
	if old.ID != oldResultID || old.Content != oldResult {
		t.Errorf("stored old tool result = %#v, want ID %d and original content", old, oldResultID)
	}
	fresh := toolElisionToolResult(t, stored, "call-new")
	if fresh.Content != "found value" {
		t.Errorf("stored fresh tool result = %q, want found value", fresh.Content)
	}
	for _, message := range stored {
		if strings.Contains(message.Content, "[Tool result elided]") {
			t.Errorf("elision marker was persisted in message %d", message.ID)
		}
	}
}

func TestMessageCompletionSummaryUsesItsStoredProtocolAndNeverPersistsSummary(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const (
		chatID       = 82
		mainModel    = "main-model"
		summaryModel = "summary-model"
	)
	oldResultID, oldResult := seedToolElisionHistory(t, database, chatID)
	insertAcceptedUser(t, database, chatID, "New question")
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: summaryModel}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: mainModel, MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, mainModel, llm.EndpointChatCompletions); err != nil {
		t.Fatalf("set main model endpoint preference: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, summaryModel, llm.EndpointMessages); err != nil {
		t.Fatalf("set summary model endpoint preference: %v", err)
	}

	summaryRequests := make(chan toolElisionMessagesRequest, 4)
	mainRequests := make(chan toolElisionChatRequest, 8)
	paths := make(chan string, 8)
	var mainRequestNumber atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
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
			var request toolElisionChatRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode main request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			mainRequests <- request
			w.Header().Set("Content-Type", "application/json")
			if mainRequestNumber.Add(1) == 1 {
				_, _ = w.Write([]byte(toolElisionToolCallResponse))
				return
			}
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
		"/messages/complete?chat=82", url.Values{
			"model":   {mainModel},
			"request": {newToolCallRequest(t, toolCalls, chatID)},
			"tool":    {"lookup"},
		})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Fresh answer")

	summaryRequest := receiveToolElisionValue(t, summaryRequests, "summary provider request")
	if summaryRequest.Model != summaryModel {
		t.Errorf("summary request model = %q, want %q", summaryRequest.Model, summaryModel)
	}
	if len(summaryRequest.Tools) != 0 {
		t.Errorf("summary request unexpectedly included %d tools", len(summaryRequest.Tools))
	}
	summaryPrompt := toolElisionMessagesText(summaryRequest)
	requireContains(t, summaryPrompt, "Tool function: lookup", "Tool arguments: {\"key\":\"old\"}", "Tool result:\n"+oldResult)

	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		request := receiveToolElisionValue(t, mainRequests, "main provider request")
		if request.Model != mainModel {
			t.Errorf("main request model = %q, want %q", request.Model, mainModel)
		}
		old := toolElisionToolResult(t, request.Messages, "old-call")
		wantSummary := "[Tool result summarized]\nEarlier result: retained key value."
		if old.Content != wantSummary {
			t.Errorf("main provider old result = %q, want summary %q", old.Content, wantSummary)
		}
		if !toolElisionAssistantHasCall(request.Messages, "old-call") {
			t.Errorf("old assistant tool-call ID was not preserved in main request %d", requestIndex+1)
		}
		if requestIndex == 1 {
			fresh := toolElisionToolResult(t, request.Messages, "call-new")
			if fresh.Content != "found value" {
				t.Errorf("fresh provider tool result = %q, want original result", fresh.Content)
			}
		}
	}
	gotPaths := []string{
		receiveToolElisionValue(t, paths, "summary request path"),
		receiveToolElisionValue(t, paths, "first main request path"),
		receiveToolElisionValue(t, paths, "second main request path"),
	}
	wantPaths := []string{"/v1/chat/messages", "/v1/chat/completions", "/v1/chat/completions"}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("provider paths = %v, want %v", gotPaths, wantPaths)
	}
	if got := mainRequestNumber.Load(); got != 2 {
		t.Errorf("main provider request count = %d, want 2", got)
	}
	if got, err := kritui_db.GetModelEndpointType(context.Background(), database, mainModel); err != nil {
		t.Fatalf("get main endpoint preference: %v", err)
	} else if got != llm.EndpointChatCompletions {
		t.Errorf("main endpoint preference = %q, want chat_completions", got)
	}
	if got, err := kritui_db.GetModelEndpointType(context.Background(), database, summaryModel); err != nil {
		t.Fatalf("get summary endpoint preference: %v", err)
	} else if got != llm.EndpointMessages {
		t.Errorf("summary endpoint preference = %q, want messages", got)
	}

	stored, err := kritui_db.GetMessages(context.Background(), database, chatID)
	if err != nil {
		t.Fatalf("get persisted messages: %v", err)
	}
	old := toolElisionToolResult(t, stored, "old-call")
	if old.ID != oldResultID || old.Content != oldResult {
		t.Errorf("stored old tool result = %#v, want ID %d and original content", old, oldResultID)
	}
	fresh := toolElisionToolResult(t, stored, "call-new")
	if fresh.Content != "found value" {
		t.Errorf("stored fresh tool result = %q, want found value", fresh.Content)
	}
	for _, message := range stored {
		if strings.Contains(message.Content, "[Tool result summarized]") || strings.Contains(message.Content, "Earlier result: retained key value.") {
			t.Errorf("summary was persisted in message %d: %q", message.ID, message.Content)
		}
	}
}

func TestMessageCompletionSummaryFailureReturnsRetryWithoutPersistingTail(t *testing.T) {
	setToolElisionCompletionEnvironment(t)
	database := openTestDatabase(t)
	const chatID = 83
	seedToolElisionHistory(t, database, chatID)
	insertAcceptedUser(t, database, chatID, "New question")
	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: "summary-model"}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: "main-model", MaxToolRounds: 16, ToolResultElision: &config,
	}); err != nil {
		t.Fatalf("save elision settings: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, "summary-model", llm.EndpointMessages); err != nil {
		t.Fatalf("set summary model endpoint preference: %v", err)
	}
	var before int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&before); err != nil {
		t.Fatalf("count messages before completion: %v", err)
	}
	providerPaths := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerPaths <- r.URL.Path
		http.Error(w, "private provider detail", http.StatusBadGateway)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "local-test-key")
	t.Setenv("LLM_MODEL", "main-model")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newToolElisionLookupRegistry(t), toolCalls, nil), toolCalls,
		"/messages/complete?chat=83", url.Values{
			"model":   {"main-model"},
			"request": {newToolCallRequest(t, toolCalls, chatID)},
			"tool":    {"lookup"},
		})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireContains(t, response.Body.String(), `class="message completion-error"`, `hx-post="/messages/retry?chat=83"`, "Retry", "Model endpoint returned HTTP 502")
	if strings.Contains(response.Body.String(), "private provider detail") {
		t.Error("completion error exposed provider response body")
	}
	if got := receiveToolElisionValue(t, providerPaths, "summary failure request path"); got != "/v1/chat/messages" {
		t.Errorf("summary failure endpoint path = %q, want /v1/chat/messages", got)
	}
	var after int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&after); err != nil {
		t.Fatalf("count messages after completion: %v", err)
	}
	if after != before {
		t.Errorf("message count after summary failure = %d, want unchanged count %d", after, before)
	}
}

func TestMigrateVersion22AddsDefaultToolResultElisionSettings(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "version22.db"))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })

	fixture := version22SchemaFixture(t)
	if _, err := database.Exec(fixture); err != nil {
		t.Fatalf("initialize version-22 schema: %v", err)
	}
	const (
		defaultModel = "legacy-model"
		llmEndpoint  = "https://llm.example/v1/responses"
		llmAPIKey    = "legacy-llm-secret"
		ntfyEndpoint = "https://notify.example"
		ntfyTopic    = "legacy-topic"
		ntfyAPIKey   = "legacy-ntfy-secret"
	)
	if _, err := database.Exec(`
		UPDATE settings SET default_model = ?, max_tool_rounds = 23, theme = 'nord',
			llm_endpoint = ?, llm_api_key = ?, ntfy_endpoint = ?, ntfy_topic = ?, ntfy_api_key = ?
		WHERE id = 1
	`, defaultModel, llmEndpoint, llmAPIKey, ntfyEndpoint, ntfyTopic, ntfyAPIKey); err != nil {
		t.Fatalf("seed version-22 settings: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(context.Background(), database, defaultModel, llm.EndpointMessages); err != nil {
		t.Fatalf("seed endpoint preference: %v", err)
	}

	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate version 22: %v", err)
	}
	assertMigratedToolElisionSettings(t, database, defaultModel, llmEndpoint, llmAPIKey, ntfyEndpoint, ntfyTopic, ntfyAPIKey)
	assertToolElisionMigrationConstraints(t, database)
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	assertMigratedToolElisionSettings(t, database, defaultModel, llmEndpoint, llmAPIKey, ntfyEndpoint, ntfyTopic, ntfyAPIKey)
}

type toolElisionChatRequest struct {
	Model    string            `json:"model"`
	Messages []llm.Message     `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

type toolElisionMessagesRequest struct {
	Model    string            `json:"model"`
	System   string            `json:"system"`
	Tools    []json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Content   string `json:"content"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	} `json:"messages"`
}

const (
	toolElisionToolCallResponse = `{"model":"provider-model","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-new","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	toolElisionFinalResponse    = `{"model":"provider-model","choices":[{"message":{"role":"assistant","content":"Fresh answer"},"finish_reason":"stop"}]}`
)

func setToolElisionSettingsEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_KEY", "")
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_ENDPOINT", "")
}

func setToolElisionCompletionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_KEY", "")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_ENDPOINT", "")
}

func toolElisionSettingsForm(config llm.ToolResultElisionConfig) url.Values {
	form := url.Values{
		"model":                    {"settings-model"},
		"max_tool_rounds":          {"16"},
		"tool_result_elision_form": {"1"},
		"tool_result_elision_mode": {string(config.Mode)},
	}
	if config.Mode == "" {
		form.Set("tool_result_elision_mode", string(llm.ToolResultElisionNone))
	}
	switch config.Mode {
	case llm.ToolResultElisionLastUserTurns:
		form.Set("tool_result_elision_user_turns", fmt.Sprint(config.UserTurns))
	case llm.ToolResultElisionBudget:
		form.Set("tool_result_elision_token_budget", fmt.Sprint(config.TokenBudget))
	case llm.ToolResultElisionSummarize:
		form.Set("tool_result_elision_summary_model", config.SummaryModel)
	}
	return form
}

func requireToolElisionModeSelected(t *testing.T, markup string, mode llm.ToolResultElisionMode) {
	t.Helper()
	optionPattern := regexp.MustCompile(`<option\b[^>]*>`)
	valuePattern := regexp.MustCompile(`\bvalue="([^"]*)"`)
	selectedPattern := regexp.MustCompile(`(?:^|\s)selected(?:\s|>|$)`)
	for _, option := range optionPattern.FindAllString(markup, -1) {
		value := valuePattern.FindStringSubmatch(option)
		if len(value) != 2 || value[1] != string(mode) {
			continue
		}
		if !selectedPattern.MatchString(option) {
			t.Errorf("elision mode %q is not selected in option %s", mode, option)
		}
		return
	}
	t.Errorf("settings markup has no option for elision mode %q", mode)
}

func newToolElisionLookupRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry, err := tools.NewRegistry(responsePersistenceTestTool{})
	if err != nil {
		t.Fatalf("create lookup registry: %v", err)
	}
	return registry
}

func seedToolElisionHistory(t *testing.T, database *sql.DB, chatID int64) (int64, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Exec(`INSERT INTO chats (id, title) VALUES (?, 'elision history')`, chatID); err != nil {
		t.Fatalf("insert history chat: %v", err)
	}
	if err := kritui_db.UpsertChat(ctx, database, chatID, "elision history", []string{"lookup"}, nil); err != nil {
		t.Fatalf("configure history chat: %v", err)
	}
	oldResult := "OLDER-PRIVATE-TOOL-OUTPUT:" + strings.Repeat(" full result details", 40)
	messages := []llm.Message{
		{Role: "user", Content: "Old question"},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID: "old-call", Type: "function",
				Function: llm.FunctionCall{Name: "lookup", Arguments: `{"key":"old"}`},
			}},
		},
		{Role: "tool", Content: oldResult, ToolCallID: "old-call"},
		{Role: "assistant", Content: "Old final answer"},
	}
	var oldResultID int64
	for position, message := range messages {
		id, err := kritui_db.InsertMessage(ctx, database, chatID, position, message)
		if err != nil {
			t.Fatalf("insert history message %d: %v", position, err)
		}
		if message.Role == "tool" {
			oldResultID = id
		}
	}
	return oldResultID, oldResult
}

func toolElisionToolResult(t *testing.T, messages []llm.Message, callID string) llm.Message {
	t.Helper()
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			return message
		}
	}
	t.Errorf("tool result for call %q is missing from messages %#v", callID, messages)
	return llm.Message{}
}

func toolElisionAssistantHasCall(messages []llm.Message, callID string) bool {
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			if call.ID == callID {
				return true
			}
		}
	}
	return false
}

func toolElisionMessagesText(request toolElisionMessagesRequest) string {
	var parts []string
	if request.System != "" {
		parts = append(parts, request.System)
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
			if block.Content != "" {
				parts = append(parts, block.Content)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func receiveToolElisionValue[T any](t *testing.T, values <-chan T, name string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func assertMigratedToolElisionSettings(t *testing.T, database *sql.DB, model, llmEndpoint, llmAPIKey, ntfyEndpoint, ntfyTopic, ntfyAPIKey string) {
	t.Helper()
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != 23 {
		t.Fatalf("schema version = %d, want 23", version)
	}
	var gotModel, gotTheme string
	var gotRounds int
	var gotLLMEndpoint, gotLLMKey, gotNtfyEndpoint, gotNtfyTopic, gotNtfyKey string
	if err := database.QueryRow(`
		SELECT default_model, max_tool_rounds, theme, llm_endpoint, llm_api_key,
			ntfy_endpoint, ntfy_topic, ntfy_api_key
		FROM settings WHERE id = 1
	`).Scan(&gotModel, &gotRounds, &gotTheme, &gotLLMEndpoint, &gotLLMKey, &gotNtfyEndpoint, &gotNtfyTopic, &gotNtfyKey); err != nil {
		t.Fatalf("read migrated settings: %v", err)
	}
	if gotModel != model || gotRounds != 23 || gotTheme != "nord" || gotLLMEndpoint != llmEndpoint || gotLLMKey != llmAPIKey || gotNtfyEndpoint != ntfyEndpoint || gotNtfyTopic != ntfyTopic || gotNtfyKey != ntfyAPIKey {
		t.Errorf("migrated settings = model %q, rounds %d, theme %q, LLM %q/%q, ntfy %q/%q/%q; seeded values were not preserved", gotModel, gotRounds, gotTheme, gotLLMEndpoint, gotLLMKey, gotNtfyEndpoint, gotNtfyTopic, gotNtfyKey)
	}
	if got, err := kritui_db.GetToolResultElision(context.Background(), database); err != nil {
		t.Fatalf("get migrated elision config: %v", err)
	} else if want := (llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone}); got != want {
		t.Errorf("migrated elision config = %#v, want %#v", got, want)
	}
	if got, err := kritui_db.GetModelEndpointType(context.Background(), database, model); err != nil {
		t.Fatalf("get preserved endpoint preference: %v", err)
	} else if got != llm.EndpointMessages {
		t.Errorf("migrated endpoint preference = %q, want messages", got)
	}
	var turns, budget sql.NullInt64
	var summary sql.NullString
	if err := database.QueryRow(`SELECT tool_result_elision_user_turns, tool_result_elision_token_budget, tool_result_elision_summary_model FROM settings WHERE id = 1`).Scan(&turns, &budget, &summary); err != nil {
		t.Fatalf("read migrated inactive values: %v", err)
	}
	if turns.Valid || budget.Valid || summary.Valid {
		t.Errorf("migrated inactive values = turns %#v, budget %#v, summary %#v; want NULLs", turns, budget, summary)
	}
}

func assertToolElisionMigrationConstraints(t *testing.T, database *sql.DB) {
	t.Helper()
	columns := map[string]struct {
		typeName string
		notNull  int
	}{}
	rows, err := database.Query(`SELECT name, type, "notnull" FROM pragma_table_info('settings')`)
	if err != nil {
		t.Fatalf("inspect migrated settings columns: %v", err)
	}
	for rows.Next() {
		var name, typeName string
		var notNull int
		if err := rows.Scan(&name, &typeName, &notNull); err != nil {
			rows.Close()
			t.Fatalf("scan migrated settings column: %v", err)
		}
		columns[name] = struct {
			typeName string
			notNull  int
		}{typeName: typeName, notNull: notNull}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close migrated settings columns: %v", err)
	}
	for _, name := range []string{"tool_result_elision_mode", "tool_result_elision_user_turns", "tool_result_elision_token_budget", "tool_result_elision_summary_model"} {
		if _, ok := columns[name]; !ok {
			t.Errorf("migrated settings column %q is missing", name)
		}
	}
	if mode := columns["tool_result_elision_mode"]; mode.typeName != "TEXT" || mode.notNull != 1 {
		t.Errorf("migrated mode column = %#v, want non-null TEXT", mode)
	}
	for _, name := range []string{"tool_result_elision_user_turns", "tool_result_elision_token_budget"} {
		if column := columns[name]; column.typeName != "INTEGER" || column.notNull != 0 {
			t.Errorf("migrated column %q = %#v, want nullable INTEGER", name, column)
		}
	}
	if summary := columns["tool_result_elision_summary_model"]; summary.typeName != "TEXT" || summary.notNull != 0 {
		t.Errorf("migrated summary column = %#v, want nullable TEXT", summary)
	}
	for _, invalid := range []struct {
		column string
		value  any
	}{
		{column: "tool_result_elision_mode", value: "unknown"},
		{column: "tool_result_elision_user_turns", value: 0},
		{column: "tool_result_elision_user_turns", value: 1001},
		{column: "tool_result_elision_token_budget", value: 0},
		{column: "tool_result_elision_token_budget", value: 10000001},
		{column: "tool_result_elision_summary_model", value: ""},
		{column: "tool_result_elision_summary_model", value: strings.Repeat("x", 513)},
	} {
		query := fmt.Sprintf(`UPDATE settings SET %s = ? WHERE id = 1`, invalid.column)
		if _, err := database.Exec(query, invalid.value); err == nil {
			t.Errorf("migrated constraint accepted %s = %v", invalid.column, invalid.value)
		}
	}
}

func version22SchemaFixture(t *testing.T) string {
	t.Helper()
	const firstNewColumn = "    tool_result_elision_mode TEXT NOT NULL DEFAULT 'none'"
	start := strings.Index(schema, firstNewColumn)
	if start < 0 {
		t.Fatal("schema does not contain tool-result elision columns")
	}
	relativeEnd := strings.Index(schema[start:], "\n) STRICT;")
	if relativeEnd < 0 {
		t.Fatal("schema settings table terminator is missing")
	}
	prefix := strings.TrimSuffix(schema[:start], ",\n")
	fixture := prefix + schema[start+relativeEnd:]
	const currentVersion = "PRAGMA user_version = 23;"
	if !strings.Contains(fixture, currentVersion) {
		t.Fatal("schema version marker is not 23")
	}
	return strings.Replace(fixture, currentVersion, "PRAGMA user_version = 22;", 1)
}
