package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReasoningEffortPayloads(t *testing.T) {
	for _, protocol := range []struct{ path, field, response string }{
		{"/responses", "reasoning", `{"model":"test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done"}]}]}`},
		{"/chat/completions", "reasoning_effort", `{"model":"test","choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`},
		{"/messages", "output_config", `{"model":"test","role":"assistant","content":[{"type":"text","text":"Done"}],"stop_reason":"end_turn"}`},
	} {
		for _, effort := range []string{"", "low", "high"} {
			t.Run(protocol.path+"/"+effort, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					for _, field := range []string{"reasoning", "reasoning_effort", "output_config"} {
						value, present := payload[field]
						if field != protocol.field || effort == "" {
							if present {
								t.Errorf("unexpected %s = %s", field, value)
							}
							continue
						}
						want := fmt.Sprintf(`{"effort":%q}`, effort)
						if field == "reasoning_effort" {
							want = fmt.Sprintf(`%q`, effort)
						}
						if string(value) != want {
							t.Errorf("%s = %s, want %s", field, value, want)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(protocol.response))
				}))
				defer server.Close()
				client, err := New("test-key", "test", server.URL+protocol.path, ClientOptions{ReasoningEffort: effort})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.CompleteText(context.Background(), "System", "Hello"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReasoningEffortSurvivesProtocolFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if r.URL.Path == "/responses" {
			if string(payload["reasoning"]) != `{"effort":"high"}` {
				t.Error("Responses lost effort")
			}
			http.Error(w, "try another protocol", http.StatusInternalServerError)
			return
		}
		if string(payload["output_config"]) != `{"effort":"high"}` {
			t.Error("Messages fallback lost effort")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"Done"}],"stop_reason":"end_turn"}`))
	}))
	defer server.Close()
	client, err := New("test-key", "test", server.URL+"/responses", ClientOptions{ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CompleteText(context.Background(), "System", "Hello"); err != nil {
		t.Fatal(err)
	}
}
