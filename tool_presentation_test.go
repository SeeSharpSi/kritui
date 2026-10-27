package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSteeringKeepsCurrentToolChainLiveAcrossCheckpoints(t *testing.T) {
	database := openTestDatabase(t)
	registry := inboxTestRegistry(t)
	commands := newTestCommandRegistry(t, database)
	store := newToolCallStore()
	const model = "tool-presentation-model"
	chatID := seedChat(t, database, "Tool presentation", []string{"inbox_test_probe"}, nil)
	gates := []*inboxTestGate{newInboxTestGate(), newInboxTestGate(), newInboxTestGate(), newInboxTestGate()}
	defer func() {
		for _, gate := range gates {
			gate.releaseNow()
		}
	}()
	provider := newInboxTestProvider(t, func(index int, _ inboxTestProviderRequest, r *http.Request, w http.ResponseWriter) {
		if index >= len(gates) {
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			return
		}
		gates[index].wait(r)
		switch index {
		case 0:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "before-steer", Name: "inbox_test_probe", Key: "old"})
		case 1:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "after-steer-one", Name: "inbox_test_probe", Key: "one"})
		case 2:
			inboxTestWriteToolCalls(w, inboxTestToolCall{ID: "after-steer-two", Name: "inbox_test_probe", Key: "two"})
		case 3:
			inboxTestWriteFinal(w, "Finished steered response")
		}
	})
	inboxTestUseProvider(t, provider, model)
	message := messageHandler(database, registry, commands, store)
	requestID := inboxTestSubmitInitial(t, message, chatID, url.Values{
		"message": {"Initial question"}, "model": {model}, "tool": {"inbox_test_probe"},
	})
	inboxTestStartCompletion(t, messageCompletionHandler(database, registry, store, nil), chatID, requestID, model, "inbox_test_probe")
	provider.waitForRequest(t, 1)
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message": {"Steering question"}, "model": {model}, "tool": {"inbox_test_probe"},
	})
	gates[0].releaseNow()
	provider.waitForRequest(t, 2)
	tracker, ok := store.get(requestID)
	if !ok {
		t.Fatal("active tracker missing")
	}
	state := tracker.snapshot()
	if len(state.calls) != 0 {
		t.Fatalf("pre-steer calls still live: %#v", state.calls)
	}
	requireContains(t, state.presentation, `class="completed-tool-calls"`, `data-tool-call-id="before-steer"`, "Steering question")

	checkLive := func(ids ...string) {
		t.Helper()
		state := tracker.snapshot()
		if len(state.calls) != len(ids) {
			t.Fatalf("live calls = %#v, want %v", state.calls, ids)
		}
		for index, id := range ids {
			if state.calls[index].ID != id {
				t.Fatalf("live call %d = %q, want %q", index, state.calls[index].ID, id)
			}
			requireNotContains(t, state.presentation, `data-tool-call-id="`+id+`"`)
		}
		response := httptest.NewRecorder()
		homeHandler(database, registry, commands, store)(response, httptest.NewRequest(http.MethodGet, "/?chat="+strconv.FormatInt(chatID, 10), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("reload status = %d; body = %s", response.Code, response.Body.String())
		}
		body := response.Body.String()
		live := strings.Index(body, `id="completion-tools-`+requestID+`"`)
		if live < 0 {
			t.Fatal("reload lost live tool list")
		}
		for _, id := range ids {
			marker := `data-tool-call-id="` + id + `"`
			if strings.Count(body, marker) != 1 || strings.Index(body, marker) < live {
				t.Fatalf("reload did not keep %q exactly once in live list", id)
			}
		}
		if strings.Count(body, `class="completed-tool-calls"`) != 1 {
			t.Fatal("current tool chain was prematurely archived on reload")
		}
	}
	gates[1].releaseNow()
	provider.waitForRequest(t, 3)
	checkLive("after-steer-one")

	// Admission and cancellation publish fresh transcript snapshots too. Neither
	// should archive checkpointed tools or drop the live list during model work.
	inboxTestEnqueueActive(t, message, chatID, url.Values{
		"message": {"Temporary queued prompt"}, "model": {model}, "tool": {"inbox_test_probe"}, "delivery": {"queue"},
	})
	checkLive("after-steer-one")
	items := inboxTestInbox(t, context.Background(), database, chatID)
	if len(items) != 1 {
		t.Fatalf("pending items = %#v, want one", items)
	}
	remove := httptest.NewRequest(http.MethodDelete, "/", nil)
	remove.SetPathValue("chat", strconv.FormatInt(chatID, 10))
	remove.SetPathValue("input", strconv.FormatInt(items[0].ID, 10))
	response := httptest.NewRecorder()
	inboxMutationHandler(database, registry, store, nil)(response, remove)
	if response.Code != http.StatusOK {
		t.Fatalf("remove status = %d; body = %s", response.Code, response.Body.String())
	}
	checkLive("after-steer-one")

	gates[2].releaseNow()
	provider.waitForRequest(t, 4)
	checkLive("after-steer-one", "after-steer-two")
	gates[3].releaseNow()
	terminal := waitForCompletionResult(t, store, requestID)
	requireContains(t, terminal, "Finished steered response")
	if strings.Count(terminal, `class="completed-tool-calls"`) != 2 {
		t.Fatal("final response did not archive both tool groups")
	}
	for _, id := range []string{"before-steer", "after-steer-one", "after-steer-two"} {
		if strings.Count(terminal, `data-tool-call-id="`+id+`"`) != 1 {
			t.Fatalf("final transcript duplicated or lost tool %q", id)
		}
	}
}
