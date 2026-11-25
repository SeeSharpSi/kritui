package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"seesharpsi/kritui/commands"
	kritui_db "seesharpsi/kritui/db"
)

func compactionProviderServer(t *testing.T, summary string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "resp-compaction",
			"model": "main-model",
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": summary}},
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestManualCompactionStoresCheckpointAndCommandReportsIt(t *testing.T) {
	database := openTestDatabase(t)
	server := compactionProviderServer(t, "## Objective\nContinue the work.")
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/responses")
	t.Setenv("LLM_MODEL", "main-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'First question'),
			(1, 1, 'assistant', 'First answer'),
			(1, 2, 'user', 'Second question'),
			(1, 3, 'assistant', 'Second answer')
	`); err != nil {
		t.Fatal(err)
	}
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: "main-model", MaxToolRounds: 16,
	}); err != nil {
		t.Fatal(err)
	}

	registry, err := newCommandRegistry(database)
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), commands.Parsed{Name: "compact"}, 1)
	if err != nil {
		t.Fatalf("execute /compact: %v", err)
	}
	if result.Body == nil || result.Status != http.StatusOK {
		t.Fatalf("compact result = %#v", result)
	}
	var body strings.Builder
	if err := result.Body.Render(context.Background(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), "Context compacted") || !strings.Contains(body.String(), "Continue the work.") {
		t.Errorf("compact body missing checkpoint: %s", body.String())
	}
	compaction, found, err := kritui_db.GetLatestChatCompaction(context.Background(), database, 1)
	if err != nil || !found {
		t.Fatalf("latest compaction = %#v, %v, %v", compaction, found, err)
	}
	if !strings.Contains(compaction.Summary, "## Objective") || compaction.KeepMessageID == 0 {
		t.Errorf("stored compaction = %#v", compaction)
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	outbound, ok := compactedHistory(snapshot.Messages, compaction)
	if !ok || len(outbound) >= len(snapshot.Messages) {
		t.Fatalf("compacted outbound history = %d messages, stored = %d, ok = %t", len(outbound), len(snapshot.Messages), ok)
	}
	if !strings.Contains(outbound[0].Content, "<conversation-checkpoint>") {
		t.Errorf("outbound checkpoint = %q", outbound[0].Content)
	}
}

func TestAutomaticCompactionThresholdAndDisable(t *testing.T) {
	database := openTestDatabase(t)
	server := compactionProviderServer(t, "## Objective\nAutomatic summary.")
	catalog := newCatalogService(t, catalogFixture(server.URL+"/v1", "main-model",
		`{"name":"Test","limit":{"context":2000}}`))
	useModelsDevService(t, catalog)
	provider := providerEntry{ID: kritui_db.EnvProviderID, Endpoint: server.URL + "/v1/responses", APIKey: "test-key"}
	long := strings.Repeat("word ", 2000)
	messages := []llmMessageFixture{
		{role: "user", content: long},
		{role: "assistant", content: long},
		{role: "user", content: "Latest question"},
		{role: "assistant", content: "Latest answer"},
	}
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	for index, message := range messages {
		if _, err := database.Exec(`INSERT INTO messages (chat_id, position, role, content) VALUES (1, ?, ?, ?)`, index, message.role, message.content); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := maybeCompactChat(context.Background(), database, 1, provider, "main-model", snapshot.Messages, false, 0)
	if err != nil {
		t.Fatalf("automatic compaction: %v", err)
	}
	if !outcome.created {
		t.Fatal("automatic compaction did not trigger at the threshold")
	}
	disabled := false
	if err := kritui_db.SaveSettings(context.Background(), database, kritui_db.SettingsUpdate{
		Model: "main-model", MaxToolRounds: 16, CompactionAuto: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := maybeCompactChat(context.Background(), database, 1, provider, "main-model", snapshot.Messages, false, 0)
	if err != nil {
		t.Fatalf("disabled compaction: %v", err)
	}
	if second.created {
		t.Fatal("automatic compaction ran while disabled")
	}
}

type llmMessageFixture struct {
	role    string
	content string
}

func TestCompactionThreshold(t *testing.T) {
	cases := []struct {
		window int
		want   int
	}{
		{0, 0},
		{100_000, 84_000},
		{128_000, 108_800},
		{1_000_000, 850_000},
		{20_000, 17_000},
	}
	for _, test := range cases {
		if got := compactionThreshold(test.window, 15); got != test.want {
			t.Errorf("compactionThreshold(%d, 15) = %d, want %d", test.window, got, test.want)
		}
	}
	if got := compactionThreshold(1_000_000, 20); got != 800_000 {
		t.Errorf("compactionThreshold(1000000, 20) = %d, want 800000", got)
	}
}

func TestSettingsCompactionAutoDefaultsOnAndCanBeDisabled(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	t.Setenv("LLM_KEY", "")
	t.Setenv("LLM_ENDPOINT", "")
	handler := settingsHandler(database, newTestToolRegistry(t))

	get := httptest.NewRecorder()
	handler(get, httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", get.Code)
	}
	requireContains(t, get.Body.String(), `id="compaction-auto"`, `name="compaction_auto" value="1" checked`,
		`name="compaction_keep_tokens" value="15000"`, `name="compaction_buffer_percent" value="15"`)

	disable := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":                     {"saved-model"},
		"max_tool_rounds":           {"16"},
		"compaction_form":           {"1"},
		"compaction_keep_tokens":    {"24000"},
		"compaction_buffer_percent": {"20"},
		"compaction_model":          {"summary-model"},
	})
	if disable.Code != http.StatusOK {
		t.Fatalf("disable status = %d; body = %q", disable.Code, disable.Body.String())
	}
	config, configErr := kritui_db.GetCompactionConfig(context.Background(), database)
	if configErr != nil || config.Auto || config.KeepTokens != 24000 || config.BufferPercent != 20 || config.Model != "summary-model" {
		t.Fatalf("compaction config after disable = %#v, %v", config, configErr)
	}

	enable := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"compaction_form": {"1"},
		"compaction_auto": {"1"},
	})
	if enable.Code != http.StatusOK {
		t.Fatalf("enable status = %d; body = %q", enable.Code, enable.Body.String())
	}
	if enabled, err := kritui_db.GetCompactionAuto(context.Background(), database); err != nil || !enabled {
		t.Fatalf("automatic compaction after enable = %v, %v", enabled, err)
	}
}

func TestCompletionUsesCompactedCheckpointAfterAutoCompaction(t *testing.T) {
	database := openTestDatabase(t)
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, body)
		answer := "Answer"
		if strings.Contains(string(body), "Summarize an ongoing conversation") {
			answer = "## Objective\nAuto compact."
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "resp-compaction",
			"model": "main-model",
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": answer}},
			}},
			"usage": map[string]int{"input_tokens": 12, "output_tokens": 4},
		})
	}))
	defer server.Close()
	useModelsDevService(t, newCatalogService(t, catalogFixture(server.URL+"/v1", "main-model",
		`{"name":"Test","limit":{"context":2000}}`)))
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/responses")
	t.Setenv("LLM_MODEL", "main-model")

	long := strings.Repeat("old context ", 2000)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', ?),
			(1, 1, 'assistant', ?)
	`, long, long); err != nil {
		t.Fatal(err)
	}
	insertAcceptedUser(t, database, 1, "New question")
	toolCalls := newToolCallStore()
	handler := messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil)
	response := completeForm(t, handler, toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"main-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("completion status = %d; body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Answer") {
		t.Fatalf("completion body = %s", response.Body.String())
	}
	if len(requests) < 2 {
		t.Fatalf("provider requests = %d, want summary and completion", len(requests))
	}
	mainBody := string(requests[len(requests)-1])
	if !strings.Contains(mainBody, "conversation-checkpoint") {
		t.Errorf("completion request did not use the compacted checkpoint: %s", mainBody[:min(len(mainBody), 500)])
	}
	if strings.Contains(mainBody, long[:100]) {
		t.Error("completion request still contained the summarized oldest message")
	}
}

func TestProviderModelListCachesContextWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model","context_length":65536}]}`))
	}))
	defer server.Close()
	provider := providerEntry{ID: 991, Endpoint: server.URL + "/v1/chat/completions", APIKey: "local"}
	models, _ := listProviderModels(context.Background(), provider, "")
	if len(models) != 1 || models[0] != "local-model" {
		t.Fatalf("models = %#v", models)
	}
	if window := modelContextWindow(context.Background(), provider, "local-model"); window != 65536 {
		t.Errorf("context window = %d, want 65536", window)
	}
}

func TestCompletionRecoversFromContextOverflowOnce(t *testing.T) {
	database := openTestDatabase(t)
	useModelsDevService(t, nil)
	var mainRequests int
	var summaryRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		if strings.Contains(text, "You summarize an ongoing conversation") {
			summaryRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "summary", "model": "main-model",
				"output": []map[string]any{{"type": "message", "role": "assistant",
					"content": []map[string]string{{"type": "output_text", "text": "## Objective\nRecover."}}}},
				"usage": map[string]int{"input_tokens": 120, "output_tokens": 20},
			})
			return
		}
		mainRequests++
		if mainRequests == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "answer", "model": "main-model",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": "Recovered answer"}}}},
			"usage": map[string]int{"input_tokens": 12, "output_tokens": 4},
		})
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/responses")
	t.Setenv("LLM_MODEL", "main-model")
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'older question'),
			(1, 1, 'assistant', 'older answer')
	`); err != nil {
		t.Fatal(err)
	}
	insertAcceptedUser(t, database, 1, "new question")
	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls,
		"/messages/complete?chat=1", url.Values{
			"model":   {"main-model"},
			"request": {newToolCallRequest(t, toolCalls, 1)},
		})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Recovered answer") {
		t.Fatalf("completion body = %q", response.Body.String())
	}
	if mainRequests != 2 || summaryRequests != 1 {
		t.Fatalf("requests: main=%d summary=%d, want 2 and 1", mainRequests, summaryRequests)
	}
	if compaction, found, err := kritui_db.GetLatestChatCompaction(context.Background(), database, 1); err != nil || !found || compaction.Summary == "" {
		t.Fatalf("overflow compaction = %#v, %v, %v", compaction, found, err)
	} else if compaction.InputTokens != 120 || compaction.OutputTokens != 20 {
		t.Errorf("compaction usage = %d in, %d out", compaction.InputTokens, compaction.OutputTokens)
	}
}

func TestProviderContextWindowEnablesAutomaticCompaction(t *testing.T) {
	useModelsDevService(t, nil)
	database := openTestDatabase(t)
	var summaryRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"local-model","context_length":2000}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "You summarize an ongoing conversation") {
			summaryRequests++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp", "model": "local-model",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": "## Objective\nLocal summary."}}}},
			"usage": map[string]int{"input_tokens": 9, "output_tokens": 3},
		})
	}))
	defer server.Close()
	provider := providerEntry{ID: kritui_db.EnvProviderID, Endpoint: server.URL + "/v1/responses", APIKey: "local"}
	if models, _ := listProviderModels(context.Background(), provider, ""); len(models) != 1 {
		t.Fatalf("models = %#v", models)
	}
	long := strings.Repeat("word ", 2000)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', ?),
			(1, 1, 'assistant', ?),
			(1, 2, 'user', 'latest')
	`, long, long); err != nil {
		t.Fatal(err)
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := maybeCompactChat(context.Background(), database, 1, provider, "local-model", snapshot.Messages, false, 0)
	if err != nil {
		t.Fatalf("automatic compaction: %v", err)
	}
	if !outcome.created || summaryRequests != 1 {
		t.Fatalf("outcome = %#v, summary requests = %d", outcome, summaryRequests)
	}
}

func TestPrecomputedCompactionIsUsedInsteadOfSecondSummary(t *testing.T) {
	useModelsDevService(t, nil)
	database := openTestDatabase(t)
	provider := providerEntry{ID: kritui_db.EnvProviderID, Endpoint: "", APIKey: "key"}
	var summaryRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "You summarize an ongoing conversation") {
			summaryRequests++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "summary", "model": "main-model",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": "## Objective\nPrecomputed."}}}},
			"usage": map[string]int{"input_tokens": 50, "output_tokens": 8},
		})
	}))
	defer server.Close()
	provider.Endpoint = server.URL + "/v1/responses"
	rememberContextWindow(provider, "main-model", 2000)
	long := strings.Repeat("word ", 500)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', ?),
			(1, 1, 'assistant', ?),
			(1, 2, 'user', 'latest')
	`, long, long); err != nil {
		t.Fatal(err)
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	precomputeCompaction(context.Background(), database, 1, provider, "main-model", snapshot.Messages, 0)
	precomputedMu.Lock()
	_, held := precomputedByChat[compactionCacheKey{database: database, chatID: 1}]
	precomputedMu.Unlock()
	if !held {
		t.Fatal("precompute did not hold a summary")
	}
	if _, err := database.Exec(`INSERT INTO messages (chat_id, position, role, content) VALUES (1, 3, 'user', ?)`, strings.Repeat("more ", 700)); err != nil {
		t.Fatal(err)
	}
	snapshot, err = kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := maybeCompactChat(context.Background(), database, 1, provider, "main-model", snapshot.Messages, false, 0)
	if err != nil {
		t.Fatalf("use precomputed compaction: %v", err)
	}
	if !outcome.created {
		t.Fatal("precomputed compaction was not applied")
	}
	if summaryRequests != 1 {
		t.Errorf("summary requests = %d, want only the precompute call", summaryRequests)
	}
}

func TestChooseKeepStartHandlesOneLongToolTurn(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'question');
	`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := database.Exec(`
			INSERT INTO messages (chat_id, position, role, content, tool_call_id)
			VALUES (1, ?, 'assistant', 'working', NULL), (1, ?, 'tool', ?, ?)
		`, index*2+1, index*2+2, strings.Repeat("tool ", 5000), "call-"+strconv.Itoa(index)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	start := chooseKeepStart(snapshot.Messages, 15000, 0)
	if start <= 0 || start >= len(snapshot.Messages) {
		t.Fatalf("keep start = %d for %d messages, want a later boundary", start, len(snapshot.Messages))
	}
	if snapshot.Messages[start].Role == "tool" {
		t.Fatalf("keep start %d begins with a tool result", start)
	}
}

func TestChooseKeepStartAdvancesPastPreviousCheckpoint(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'first question'),
			(1, 1, 'assistant', 'first answer'),
			(1, 2, 'user', 'second question');
	`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := database.Exec(`
			INSERT INTO messages (chat_id, position, role, content, tool_call_id)
			VALUES (1, ?, 'assistant', 'working', NULL), (1, ?, 'tool', ?, ?)
		`, index*2+3, index*2+4, strings.Repeat("tool ", 5000), "call-"+strconv.Itoa(index)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	previousStart := 2
	start := chooseKeepStart(snapshot.Messages, 15000, previousStart)
	if start <= previousStart {
		t.Fatalf("keep start = %d, previous = %d; compaction would not advance", start, previousStart)
	}
}

func TestForcedCompactionHandlesOneLongToolTurn(t *testing.T) {
	database := openTestDatabase(t)
	server := compactionProviderServer(t, "## Objective\nContinue after the tool work.")
	provider := providerEntry{ID: kritui_db.EnvProviderID, Endpoint: server.URL + "/v1/responses", APIKey: "test-key"}
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'question');
	`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := database.Exec(`
			INSERT INTO messages (chat_id, position, role, content, tool_call_id)
			VALUES (1, ?, 'assistant', 'working', NULL), (1, ?, 'tool', ?, ?)
		`, index*2+1, index*2+2, strings.Repeat("tool ", 5000), "call-"+strconv.Itoa(index)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := kritui_db.GetMessageSnapshot(context.Background(), database, 1)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := maybeCompactChat(context.Background(), database, 1, provider, "main-model", snapshot.Messages, true, 0)
	if err != nil {
		t.Fatalf("force compaction: %v", err)
	}
	if !outcome.created {
		t.Fatal("forced compaction did not create a checkpoint")
	}
	var keepRole string
	if err := database.QueryRow(`SELECT role FROM messages WHERE id = ?`, outcome.compaction.KeepMessageID).Scan(&keepRole); err != nil {
		t.Fatal(err)
	}
	if keepRole == "tool" {
		t.Fatalf("checkpoint keeps tool message %d without its assistant call", outcome.compaction.KeepMessageID)
	}
	if len(outcome.messages) >= len(snapshot.Messages) {
		t.Fatalf("outbound messages = %d, stored = %d; compaction did not shorten context", len(outcome.messages), len(snapshot.Messages))
	}
}
