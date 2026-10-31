package modelsdev

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

const testCatalog = `{
  "opencode": {
    "id": "opencode",
    "name": "OpenCode",
    "api": "https://opencode.ai/inference/openai/v1",
    "models": {
      "deepseek-v4.1-flash": {
        "name": "DeepSeek V4.1 Flash",
        "canonical_model_id": "deepseek/deepseek-v4.1-flash",
        "reasoning": true,
        "reasoning_options": [
          {"type": "toggle"},
          {"type": "effort", "values": ["low", "high", "max"]}
        ],
        "tool_call": true,
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "limit": {"context": 1000000, "output": 384000},
        "cost": {"input": 0.3, "output": 1.2, "cache_read": 0.006}
      }
    }
  },
  "opencode-go": {
    "id": "opencode-go",
    "name": "OpenCode Go",
    "api": "https://opencode.ai/inference/go/openai/v1",
    "models": {
      "aaa-model": {
        "name": "AAA Model",
        "tool_call": false,
        "modalities": {"input": ["text"], "output": ["text"]},
        "cost": {"input": 1, "output": 2}
      },
      "image-model": {
        "name": "Image Model",
        "modalities": {"input": ["text"], "output": ["image"]}
      },
      "deepseek-v4.1-flash": {
        "name": "DeepSeek V4.1 Flash",
        "canonical_model_id": "deepseek/deepseek-v4.1-flash",
        "reasoning": true,
        "reasoning_options": [{"type": "effort", "values": ["low", "high", "max"]}],
        "tool_call": true,
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "limit": {"context": 1000000, "output": 384000},
        "cost": {"input": 0.15, "output": 0.6, "cache_read": 0.003}
      }
    }
  },
  "relay": {
    "id": "relay",
    "name": "Relay",
    "api": "https://relay.example.com/v1",
    "models": {
      "ds-alias": {
        "name": "Alias",
        "canonical_model_id": "deepseek/deepseek-v4.1-flash",
        "modalities": {"input": ["text", "pdf"], "output": ["text"]}
      }
    }
  }
}`

func newTestService(t *testing.T, body string) (*Service, *MemoryCache) {
	t.Helper()
	cache := &MemoryCache{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"catalog-1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	service, err := NewService(cache, Options{URL: server.URL, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewService() error: %v", err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	return service, cache
}

func TestLookupMatchesProviderAndCanonicalModel(t *testing.T) {
	service, _ := newTestService(t, testCatalog)

	got, ok := service.Lookup("https://opencode.ai/inference/go/openai/v1/responses", "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("Lookup() did not find opencode-go model")
	}
	if got.ProviderID != "opencode-go" || got.ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("Lookup() = %#v, want opencode-go/deepseek-v4.1-flash", got)
	}
	if cost, ok := got.Model.EstimateCost(1_000_000, 0, 0, 0); !ok || cost != 0.15 {
		t.Fatalf("opencode-go input cost = %v, %v, want 0.15, true", cost, ok)
	}

	got, ok = service.Lookup("https://opencode.ai/inference/openai/v1/responses", "deepseek-v4.1-flash")
	if !ok || got.ProviderID != "opencode" {
		t.Fatalf("Lookup() provider = %#v, %v, want opencode", got, ok)
	}
	if cost, _ := got.Model.EstimateCost(1_000_000, 0, 0, 0); cost != 0.3 {
		t.Fatalf("opencode input cost = %v, want 0.3", cost)
	}

	got, ok = service.Lookup("https://relay.example.com/v1/chat/completions", "deepseek-v4.1-flash")
	if !ok || got.ModelID != "ds-alias" {
		t.Fatalf("canonical fallback = %#v, %v, want ds-alias", got, ok)
	}
	if image, known := got.Model.SupportsImage(); image || !known {
		t.Errorf("image support = %v, %v, want false, true", image, known)
	}
	if pdf, known := got.Model.SupportsPDF(); !pdf || !known {
		t.Errorf("PDF support = %v, %v, want true, true", pdf, known)
	}

	if _, ok := service.Lookup("https://unknown.example.com/v1", "deepseek-v4.1-flash"); ok {
		t.Fatal("Lookup() matched an unknown endpoint")
	}
	if _, ok := service.Lookup("https://opencode.ai/inference/openai/v1", "missing"); ok {
		t.Fatal("Lookup() matched an unknown model")
	}
}

func TestModelsListsProviderTextModels(t *testing.T) {
	service, _ := newTestService(t, testCatalog)

	models, ok := service.Models("https://opencode.ai/inference/go/openai/v1/chat/completions")
	if !ok {
		t.Fatal("Models() did not resolve opencode-go")
	}
	want := []string{"aaa-model", "deepseek-v4.1-flash"}
	if len(models) != len(want) {
		t.Fatalf("Models() = %v, want %v", models, want)
	}
	for index := range want {
		if models[index] != want[index] {
			t.Fatalf("Models() = %v, want %v", models, want)
		}
	}
	if _, ok := service.Models("https://unknown.example.com/v1"); ok {
		t.Fatal("Models() matched an unknown endpoint")
	}
}

func TestModelCapabilities(t *testing.T) {
	model := Model{Modalities: Modalities{Input: []string{"text", "image/png"}}}
	if image, known := model.SupportsImage(); !image || !known {
		t.Errorf("SupportsImage() = %v, %v, want true, true", image, known)
	}
	if pdf, known := model.SupportsPDF(); pdf || !known {
		t.Errorf("SupportsPDF() = %v, %v, want false, true", pdf, known)
	}
	if text, known := model.SupportsInput("text"); !text || !known {
		t.Errorf("SupportsInput(text) = %v, %v", text, known)
	}
	if audio, known := model.SupportsInput("audio"); audio || !known {
		t.Errorf("SupportsInput(audio) = %v, %v", audio, known)
	}

	empty := Model{}
	if _, known := empty.SupportsImage(); known {
		t.Error("empty modalities should be unknown")
	}
	document := Model{Modalities: Modalities{Input: []string{"application/pdf", "audio"}}}
	if pdf, known := document.SupportsPDF(); !pdf || !known {
		t.Errorf("document SupportsPDF() = %v, %v", pdf, known)
	}
	if audio, known := document.SupportsInput("audio"); !audio || !known {
		t.Errorf("document SupportsInput(audio) = %v, %v", audio, known)
	}
}

func TestReasoningMetadata(t *testing.T) {
	model := Model{
		Reasoning: true,
		ReasoningOptions: []ReasoningOption{
			{Type: "toggle"},
			{Type: "effort", Values: []string{"low", "high", "low", "max"}},
			{Type: "budget_tokens"},
		},
	}
	if got := model.ReasoningEfforts(); len(got) != 3 || got[0] != "low" || got[2] != "max" {
		t.Fatalf("ReasoningEfforts() = %v", got)
	}
	if !model.SupportsReasoningToggle() || !model.SupportsReasoningBudget() {
		t.Error("expected toggle and budget support")
	}
	if err := model.ValidateReasoningEffort("HIGH"); err != nil {
		t.Errorf("ValidateReasoningEffort(HIGH) error = %v", err)
	}
	if err := model.ValidateReasoningEffort("ultra"); err == nil {
		t.Error("ValidateReasoningEffort(ultra) error = nil, want error")
	}
	if err := (Model{}).ValidateReasoningEffort("low"); err == nil {
		t.Error("ValidateReasoningEffort() on model without options = nil, want error")
	}
	if (Model{Reasoning: true, ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}).SupportsReasoningBudget() {
		t.Error("toggle-only model reported budget support")
	}
}

func float64Pointer(value float64) *float64 { return &value }

func TestEstimateCost(t *testing.T) {
	model := Model{Cost: &Cost{
		Input:      float64Pointer(2),
		Output:     float64Pointer(10),
		CacheRead:  float64Pointer(0.5),
		CacheWrite: float64Pointer(2.5),
	}}
	got, ok := model.EstimateCost(1_000_000, 200_000, 100_000, 100_000)
	if !ok || got != 2.75 {
		t.Fatalf("EstimateCost() = %v, %v, want 2.75, true", got, ok)
	}

	model.Cost.Tiers = []CostTier{{
		Input:      float64Pointer(4),
		Output:     float64Pointer(12),
		CacheRead:  float64Pointer(1),
		CacheWrite: float64Pointer(4),
		Tier: struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		}{Type: "context", Size: 200_000},
	}}
	got, ok = model.EstimateCost(300_000, 0, 0, 10_000)
	if !ok || got != 1.32 {
		t.Fatalf("tier EstimateCost() = %v, %v, want 1.32, true", got, ok)
	}

	legacy := Model{Cost: &Cost{
		Input:  float64Pointer(2),
		Output: float64Pointer(6),
		ContextOver200K: &Cost{
			Input:  float64Pointer(4),
			Output: float64Pointer(12),
		},
	}}
	got, ok = legacy.EstimateCost(300_000, 0, 0, 0)
	if !ok || got != 1.2 {
		t.Fatalf("context-over estimate = %v, %v, want 1.2, true", got, ok)
	}

	missingOutput := Model{Cost: &Cost{Input: float64Pointer(1)}}
	if _, ok := missingOutput.EstimateCost(1, 0, 0, 1); ok {
		t.Error("incomplete pricing estimated successfully")
	}
	zero := Model{Cost: &Cost{Input: float64Pointer(0), Output: float64Pointer(0)}}
	if cost, ok := zero.EstimateCost(1_000_000, 0, 0, 1_000_000); !ok || cost != 0 {
		t.Errorf("zero pricing = %v, %v, want 0, true", cost, ok)
	}
	if _, ok := (Model{}).EstimateCost(1, 0, 0, 1); ok {
		t.Error("model without cost estimated successfully")
	}

	fallbackCache := Model{Cost: &Cost{Input: float64Pointer(2), Output: float64Pointer(10)}}
	got, ok = fallbackCache.EstimateCost(1_000_000, 500_000, 0, 0)
	if !ok || got != 2.0 {
		t.Fatalf("cache fallback estimate = %v, %v, want 2.0, true", got, ok)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://opencode.ai/inference/go/openai/v1/responses", "https://opencode.ai/inference/go/openai/v1"},
		{"https://api.example.com/v1/chat/completions", "https://api.example.com/v1"},
		{"https://api.example.com/v1/messages/", "https://api.example.com/v1"},
		{"https://api.example.com/v1/chat/messages", "https://api.example.com/v1"},
		{"HTTPS://API.EXAMPLE.COM:443/v1/responses?x=1#frag", "https://api.example.com/v1"},
		{"http://api.example.com:80", "http://api.example.com"},
		{"not-a-url", ""},
		{"ftp://api.example.com/v1", ""},
	}
	for _, test := range tests {
		if got := NormalizeEndpoint(test.raw); got != test.want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}

type conditionalServer struct {
	requests int
	etags    []string
}

func newConditionalCatalogServer(t *testing.T, body string) (*httptest.Server, *conditionalServer) {
	t.Helper()
	state := &conditionalServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.requests++
		state.etags = append(state.etags, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, state
}

func TestRefreshConditionalAndCacheRoundTrip(t *testing.T) {
	server, state := newConditionalCatalogServer(t, testCatalog)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cache := &MemoryCache{}
	service, err := NewService(cache, Options{URL: server.URL, Now: func() time.Time { return now }, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh() error: %v", err)
	}
	if service.Stale() {
		t.Error("fresh catalog reported stale")
	}

	now = now.Add(time.Hour)
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("conditional Refresh() error: %v", err)
	}
	if state.requests != 2 || state.etags[0] != "" || state.etags[1] != `"v1"` {
		t.Fatalf("requests = %d, etags = %#v", state.requests, state.etags)
	}
	entry, err := cache.Load(context.Background())
	if err != nil || entry.ETag != `"v1"` || !entry.FetchedAt.Equal(now) {
		t.Fatalf("cached entry = %#v, %v", entry, err)
	}

	reloaded, err := NewService(cache, Options{URL: server.URL, Now: func() time.Time { return now }, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if _, ok := reloaded.Lookup("https://opencode.ai/inference/openai/v1", "deepseek-v4.1-flash"); !ok {
		t.Fatal("reloaded catalog did not resolve model")
	}
}

func TestDefaultCacheTTL(t *testing.T) {
	server, _ := newConditionalCatalogServer(t, testCatalog)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	service, err := NewService(nil, Options{URL: server.URL, Now: func() time.Time { return now }, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Hour)
	if service.Stale() {
		t.Error("catalog reported stale before 6 hours")
	}
	now = now.Add(2 * time.Hour)
	if !service.Stale() {
		t.Error("catalog reported fresh after 7 hours")
	}
}

func TestRefreshKeepsPreviousCatalogOnMalformedPayload(t *testing.T) {
	body := testCatalog
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	service, err := NewService(nil, Options{URL: server.URL, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	body = `{"broken":`
	if err := service.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() malformed payload error = nil")
	}
	if _, ok := service.Lookup("https://opencode.ai/inference/openai/v1", "deepseek-v4.1-flash"); !ok {
		t.Fatal("malformed refresh replaced the previous catalog")
	}
}

func TestSQLCacheRoundTrip(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE models_dev_cache (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			etag TEXT NOT NULL DEFAULT '',
			fetched_at INTEGER NOT NULL,
			payload BLOB NOT NULL
		) STRICT
	`); err != nil {
		t.Fatal(err)
	}
	cache := NewSQLCache(database)
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrCacheEmpty) {
		t.Fatalf("empty Load() error = %v, want ErrCacheEmpty", err)
	}
	want := CacheEntry{Payload: []byte(testCatalog), ETag: `"v1"`, FetchedAt: time.Unix(1_700_000_000, 0).UTC()}
	if err := cache.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(want.Payload) || got.ETag != want.ETag || !got.FetchedAt.Equal(want.FetchedAt) {
		t.Fatalf("SQLCache round trip = %#v", got)
	}
}

func TestStartLoadsCatalogInBackground(t *testing.T) {
	server, _ := newConditionalCatalogServer(t, testCatalog)
	service, err := NewService(nil, Options{URL: server.URL, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	service.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := service.Lookup("https://opencode.ai/inference/openai/v1", "deepseek-v4.1-flash"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Start() did not load the catalog in time")
}

func TestLiveCatalog(t *testing.T) {
	if os.Getenv("MODELSDEV_LIVE") != "1" {
		t.Skip("set MODELSDEV_LIVE=1 to fetch the public catalog")
	}
	service, err := NewService(nil, Options{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.Refresh(ctx); err != nil {
		t.Fatalf("live Refresh() error: %v", err)
	}
	providerModel, ok := service.Lookup("https://opencode.ai/inference/go/openai/v1/responses", "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("live catalog did not resolve opencode-go deepseek-v4.1-flash")
	}
	if providerModel.ProviderID != "opencode-go" {
		t.Fatalf("live provider = %q, want opencode-go", providerModel.ProviderID)
	}
	if image, known := providerModel.Model.SupportsImage(); !image || !known {
		t.Errorf("live image support = %v, %v", image, known)
	}
	if cost, ok := providerModel.Model.EstimateCost(1_000_000, 0, 0, 0); !ok || cost <= 0 {
		t.Errorf("live input cost = %v, %v", cost, ok)
	}
}
