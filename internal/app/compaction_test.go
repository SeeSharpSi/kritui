package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if !strings.Contains(body.String(), "Compacted") || !strings.Contains(body.String(), "checkpoint") {
		t.Errorf("compact body missing notice: %s", body.String())
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
		if got := compactionThreshold(test.window); got != test.want {
			t.Errorf("compactionThreshold(%d) = %d, want %d", test.window, got, test.want)
		}
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
	requireContains(t, get.Body.String(), `id="compaction-auto"`, `name="compaction_auto" value="1" checked`)

	disable := postForm(t, handler, "/settings?chat=8", url.Values{
		"model":           {"saved-model"},
		"max_tool_rounds": {"16"},
		"compaction_form": {"1"},
	})
	if disable.Code != http.StatusOK {
		t.Fatalf("disable status = %d; body = %q", disable.Code, disable.Body.String())
	}
	if enabled, err := kritui_db.GetCompactionAuto(context.Background(), database); err != nil || enabled {
		t.Fatalf("automatic compaction after disable = %v, %v", enabled, err)
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
