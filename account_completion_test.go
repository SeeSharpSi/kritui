package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

type accountCompletionNtfyObservation struct {
	method        string
	path          string
	authorization string
	title         string
	message       string
}

func TestNonOwnerAsyncCompletionLifetimeIsolation(t *testing.T) {
	t.Setenv("LLM_KEY", "")
	t.Setenv("LLM_ENDPOINT", "")
	t.Setenv("LLM_MODEL", "")

	database := openTestDatabase(t)
	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)

	var ownerProviderRequests atomic.Int32
	ownerProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ownerProviderRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"owner-model",
			"choices":[{"message":{"role":"assistant","content":"OWNER PROVIDER LEAK"},"finish_reason":"stop"}]
		}`)
	}))
	defer ownerProvider.Close()

	var ownerNtfyRequests atomic.Int32
	ownerNtfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ownerNtfyRequests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ownerNtfy.Close()

	user2ProviderStarted := make(chan struct{})
	releaseUser2Provider := make(chan struct{})
	var releaseUser2ProviderOnce sync.Once
	releaseUser2 := func() {
		releaseUser2ProviderOnce.Do(func() { close(releaseUser2Provider) })
	}

	var user2ProviderOnce sync.Once
	var user2ProviderMu sync.Mutex
	var user2ProviderAuthorization, user2ProviderPath string
	var user2ModelRequests atomic.Int32
	user2Provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			user2ModelRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"shared-model"}]}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}

		user2ProviderMu.Lock()
		user2ProviderAuthorization = r.Header.Get("Authorization")
		user2ProviderPath = r.URL.Path
		user2ProviderMu.Unlock()

		first := false
		user2ProviderOnce.Do(func() {
			first = true
			close(user2ProviderStarted)
		})
		if first {
			<-releaseUser2Provider
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"shared-model",
			"choices":[{"message":{"role":"assistant","content":"User2 private answer."},"finish_reason":"stop"}]
		}`)
	}))
	defer user2Provider.Close()
	defer releaseUser2()

	user2NtfyObservations := make(chan accountCompletionNtfyObservation, 1)
	user2Ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user2NtfyObservations <- accountCompletionNtfyObservation{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			title:         r.Header.Get("Title"),
			message:       string(body),
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer user2Ntfy.Close()

	if _, err := database.Exec(`
		UPDATE settings
		SET default_model = 'owner-model',
			llm_endpoint = ?,
			llm_api_key = 'owner-llm-secret',
			ntfy_endpoint = ?,
			ntfy_topic = 'owner-topic',
			ntfy_api_key = 'owner-ntfy-secret'
		WHERE id = 1
	`, ownerProvider.URL+"/chat/completions", ownerNtfy.URL); err != nil {
		t.Fatalf("seed owner settings: %v", err)
	}
	if err := kritui_db.SetModelEndpointType(ownerContext, database, "shared-model", llm.EndpointMessages); err != nil {
		t.Fatalf("seed owner endpoint preference: %v", err)
	}
	ownerChatID, err := kritui_db.AllocateChat(ownerContext, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate owner chat: %v", err)
	}
	if _, err := database.Exec(`UPDATE chats SET title = 'Owner private chat' WHERE id = ?`, ownerChatID); err != nil {
		t.Fatalf("title owner chat: %v", err)
	}
	if _, err := kritui_db.InsertMessage(ownerContext, database, ownerChatID, 0, llm.Message{
		Role:    "user",
		Content: "Owner private question.",
	}); err != nil {
		t.Fatalf("insert owner message: %v", err)
	}

	toolCalls := newToolCallStore()
	handler := newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		toolCalls,
		nil,
		newAuthService(database, false),
	)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newAuthTestClient()

	const user2Name = "Completion_User"
	_, user2Cookie := authSignup(t, client, server.URL, user2Name, authTestPassword, "")
	_, otherCookie := authSignup(t, client, server.URL, "Completion_Other", authTestPassword, "")
	user2, err := kritui_db.GetUserByUsername(context.Background(), database, user2Name)
	if err != nil {
		t.Fatalf("look up signed-up account: %v", err)
	}
	if user2.ID == kritui_db.LegacyOwnerID {
		t.Fatal("public signup received the reserved owner account")
	}
	other, err := kritui_db.GetUserByUsername(context.Background(), database, "Completion_Other")
	if err != nil {
		t.Fatalf("look up other account: %v", err)
	}

	user2Context := kritui_db.WithUser(context.Background(), user2.ID)
	user2ChatID, err := kritui_db.AllocateChat(user2Context, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate user2 chat: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE settings
		SET default_model = 'shared-model',
			llm_endpoint = ?,
			llm_api_key = 'user2-llm-secret',
			ntfy_endpoint = ?,
			ntfy_topic = 'user2-topic',
			ntfy_api_key = 'user2-ntfy-secret'
		WHERE id = ?
	`, user2Provider.URL+"/chat/completions", user2Ntfy.URL, user2.ID); err != nil {
		t.Fatalf("seed user2 settings: %v", err)
	}

	chatIDText := strconv.FormatInt(user2ChatID, 10)
	messageResponse, messageBody := authPostForm(t, client, server.URL+"/messages?chat="+chatIDText, url.Values{
		"message": {"User2 private question."},
		"model":   {"shared-model"},
	}, user2Cookie, nil)
	if messageResponse.StatusCode != http.StatusOK {
		t.Fatalf("user2 message status = %d, want %d; body = %q", messageResponse.StatusCode, http.StatusOK, messageBody)
	}

	pending, ok := toolCalls.activeForUser(user2.ID, user2ChatID)
	if !ok {
		t.Fatal("user2 pending completion is missing from the owning account")
	}
	requestID := pending.requestID
	defer toolCalls.delete(requestID)

	completionContext, cancelCompletion := context.WithCancel(context.Background())
	defer cancelCompletion()
	completionRequest, err := http.NewRequestWithContext(
		completionContext,
		http.MethodPost,
		server.URL+"/messages/complete?chat="+chatIDText,
		strings.NewReader(url.Values{
			"model":   {"shared-model"},
			"request": {requestID},
		}.Encode()),
	)
	if err != nil {
		t.Fatalf("new completion request: %v", err)
	}
	completionRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	completionRequest.AddCookie(user2Cookie)
	completionResponse, completionBody := authDo(t, client, completionRequest)
	if completionResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("completion start status = %d, want %d; body = %q", completionResponse.StatusCode, http.StatusNoContent, completionBody)
	}
	cancelCompletion()

	waitForTestSignal(t, user2ProviderStarted, "user2 provider request after request cancellation")
	user2ProviderMu.Lock()
	authorization := user2ProviderAuthorization
	providerPath := user2ProviderPath
	user2ProviderMu.Unlock()
	if authorization != "Bearer user2-llm-secret" {
		t.Errorf("user2 provider Authorization = %q, want user2 credential", authorization)
	}
	if !strings.HasSuffix(providerPath, "/chat/completions") {
		t.Errorf("user2 provider path = %q, want chat completions", providerPath)
	}
	releaseUser2()

	terminal := waitForOwnedCompletionResult(t, toolCalls, user2.ID, requestID)
	requireContains(t, terminal, "User2 private answer.")
	requireNotContains(t, terminal, "OWNER PROVIDER LEAK")

	var ntfyObservation accountCompletionNtfyObservation
	select {
	case ntfyObservation = <-user2NtfyObservations:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for user2 ntfy notification")
	}
	if ntfyObservation.method != http.MethodPost || ntfyObservation.path != "/user2-topic" {
		t.Errorf("user2 ntfy request = %s %q, want POST /user2-topic", ntfyObservation.method, ntfyObservation.path)
	}
	if ntfyObservation.authorization != "Bearer user2-ntfy-secret" {
		t.Errorf("user2 ntfy Authorization = %q, want user2 token", ntfyObservation.authorization)
	}
	if ntfyObservation.title != "Kritui response ready" {
		t.Errorf("user2 ntfy title = %q, want response title", ntfyObservation.title)
	}
	if want := "Chat " + chatIDText + " has a response."; ntfyObservation.message != want {
		t.Errorf("user2 ntfy message = %q, want metadata %q", ntfyObservation.message, want)
	}
	requireNotContains(t, ntfyObservation.message, "User2 private answer.", "User2 private question.")

	if calls := ownerProviderRequests.Load(); calls != 0 {
		t.Errorf("owner provider received %d request(s), want 0", calls)
	}
	if calls := ownerNtfyRequests.Load(); calls != 0 {
		t.Errorf("owner ntfy received %d request(s), want 0", calls)
	}

	user2Messages, err := kritui_db.GetMessages(user2Context, database, user2ChatID)
	if err != nil {
		t.Fatalf("load user2 messages: %v", err)
	}
	if len(user2Messages) != 2 {
		t.Fatalf("user2 message count = %d, want 2", len(user2Messages))
	}
	if user2Messages[0].Role != "user" || user2Messages[0].Content != "User2 private question." {
		t.Errorf("user2 user message = %#v", user2Messages[0])
	}
	if user2Messages[1].Role != "assistant" || user2Messages[1].Content != "User2 private answer." {
		t.Errorf("user2 assistant message = %#v", user2Messages[1])
	}

	ownerMessages, err := kritui_db.GetMessages(ownerContext, database, ownerChatID)
	if err != nil {
		t.Fatalf("load owner messages: %v", err)
	}
	if len(ownerMessages) != 1 || ownerMessages[0].Role != "user" || ownerMessages[0].Content != "Owner private question." {
		t.Errorf("owner messages changed: %#v", ownerMessages)
	}
	var ownerTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = ?`, ownerChatID).Scan(&ownerTitle); err != nil {
		t.Fatalf("load owner chat title: %v", err)
	}
	if ownerTitle != "Owner private chat" {
		t.Errorf("owner chat title = %q, want unchanged title", ownerTitle)
	}

	var ownerDefaultModel, ownerEndpoint, ownerKey, ownerTopic, ownerToken string
	if err := database.QueryRow(`
		SELECT default_model, llm_endpoint, llm_api_key, ntfy_topic, ntfy_api_key
		FROM settings WHERE id = 1
	`).Scan(&ownerDefaultModel, &ownerEndpoint, &ownerKey, &ownerTopic, &ownerToken); err != nil {
		t.Fatalf("load owner settings: %v", err)
	}
	if ownerDefaultModel != "owner-model" ||
		ownerEndpoint != ownerProvider.URL+"/chat/completions" ||
		ownerKey != "owner-llm-secret" ||
		ownerTopic != "owner-topic" ||
		ownerToken != "owner-ntfy-secret" {
		t.Errorf("owner settings changed: model=%q endpoint=%q key=%q topic=%q token=%q",
			ownerDefaultModel, ownerEndpoint, ownerKey, ownerTopic, ownerToken)
	}

	ownerPreference, err := kritui_db.GetModelEndpointType(ownerContext, database, "shared-model")
	if err != nil {
		t.Fatalf("load owner endpoint preference: %v", err)
	}
	if ownerPreference != llm.EndpointMessages {
		t.Errorf("owner endpoint preference = %q, want %q", ownerPreference, llm.EndpointMessages)
	}
	user2Preference, err := kritui_db.GetModelEndpointType(user2Context, database, "shared-model")
	if err != nil {
		t.Fatalf("load user2 endpoint preference: %v", err)
	}
	if user2Preference != llm.EndpointChatCompletions {
		t.Errorf("user2 endpoint preference = %q, want %q", user2Preference, llm.EndpointChatCompletions)
	}

	terminalStatus, terminalBody := readCompletionSSE(t, client, server.URL+"/messages/tools?request="+requestID, user2Cookie)
	if terminalStatus != http.StatusOK {
		t.Fatalf("user2 terminal SSE status = %d, want %d; body = %q", terminalStatus, http.StatusOK, terminalBody)
	}
	requireContains(t, terminalBody, "User2 private answer.", "#completion-"+requestID)
	requireNotContains(t, terminalBody, "OWNER PROVIDER LEAK")

	foreignStatus, foreignBody := readCompletionSSE(t, client, server.URL+"/messages/tools?request="+requestID, otherCookie)
	unknownStatus, unknownBody := readCompletionSSE(t, client, server.URL+"/messages/tools?request=unknown-request", otherCookie)
	if foreignStatus != unknownStatus || foreignBody != unknownBody {
		t.Errorf("foreign SSE = %d %q, unknown SSE = %d %q; want identical close responses",
			foreignStatus, foreignBody, unknownStatus, unknownBody)
	}
	requireContains(t, foreignBody, "event: "+toolCallCloseEvent)
	requireNotContains(t, foreignBody, "User2 private answer.", "User2 private question.", requestID)

	if other.ID == user2.ID {
		t.Fatal("other account resolved to user2")
	}
}

func waitForOwnedCompletionResult(t *testing.T, toolCalls *toolCallStore, owner int64, requestID string) string {
	t.Helper()
	tracker, ok := toolCalls.getForUser(owner, requestID)
	if !ok {
		t.Fatalf("completion tracker %q is missing for its owner", requestID)
	}

	timeout := time.NewTimer(5 * time.Second)
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

func readCompletionSSE(t *testing.T, client *http.Client, target string, cookie *http.Cookie) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform SSE request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read SSE response: %v", err)
	}
	return response.StatusCode, string(body)
}
