package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/modelsdev"
)

func newCatalogService(t *testing.T, body string) *modelsdev.Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	cache := &modelsdev.MemoryCache{}
	service, err := modelsdev.NewService(cache, modelsdev.Options{
		URL:    server.URL,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return service
}

func useModelsDevService(t *testing.T, service *modelsdev.Service) {
	t.Helper()
	previous := modelsDevService
	modelsDevService = service
	t.Cleanup(func() { modelsDevService = previous })
}

func catalogFixture(providerAPI, modelID string, modelFields string) string {
	return `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": ` + jsonString(providerAPI) + `,
			"models": {
				` + jsonString(modelID) + `: ` + modelFields + `
			}
		}
	}`
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

const modelWithFullMetadata = `{
	"name": "Catalog Model",
	"reasoning": true,
	"tool_call": true,
	"modalities": {"input": ["text", "image"], "output": ["text"]},
	"limit": {"context": 1000000, "output": 384000},
	"cost": {"input": 0.15, "output": 0.6, "cache_read": 0.003}
}`

func TestCatalogModelSupportFallback(t *testing.T) {
	service := newCatalogService(t, catalogFixture("https://catalog.example.com/v1", "test-model", modelWithFullMetadata))
	useModelsDevService(t, service)

	info, known := catalogModelSupport("https://catalog.example.com/v1/responses", "test-model")
	if !known {
		t.Fatal("catalogModelSupport() known = false")
	}
	if info.ImageSupport != llm.InputSupportSupported || info.PDFSupport != llm.InputSupportUnsupported {
		t.Fatalf("catalog support = %#v", info)
	}

	merged, known := mergeModelSupport(
		llm.ModelInfo{ID: "test-model", ImageSupport: llm.InputSupportUnsupported},
		true,
		info,
		true,
	)
	if !known || merged.ImageSupport != llm.InputSupportUnsupported || merged.PDFSupport != llm.InputSupportUnsupported {
		t.Fatalf("provider-authoritative merge = %#v, %v", merged, known)
	}
}

func TestModelSupportChecksProviderThenCatalog(t *testing.T) {
	var providerAPI string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"test-model","capabilities":{"image_input":{"supported":false}}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"test-model","output":[]}`)
	}))
	t.Cleanup(provider.Close)
	providerAPI = provider.URL + "/v1"

	service := newCatalogService(t, catalogFixture(providerAPI, "test-model", modelWithFullMetadata))
	useModelsDevService(t, service)

	client, err := llm.New("key", "test-model", providerAPI+"/responses")
	if err != nil {
		t.Fatal(err)
	}
	info, known := modelSupportWithClient(context.Background(), client, "test-model", providerAPI+"/responses")
	if !known {
		t.Fatal("modelSupportWithClient() known = false")
	}
	if info.ImageSupport != llm.InputSupportUnsupported {
		t.Errorf("image support = %v, want provider-declared unsupported", info.ImageSupport)
	}
	if info.PDFSupport != llm.InputSupportUnsupported {
		t.Errorf("PDF support = %v, want catalog-declared unsupported", info.PDFSupport)
	}
}

func TestModelSupportFallsBackWhenProviderModelsFail(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	providerAPI := provider.URL + "/v1"
	service := newCatalogService(t, catalogFixture(providerAPI, "test-model", modelWithFullMetadata))
	useModelsDevService(t, service)

	client, err := llm.New("key", "test-model", providerAPI+"/responses")
	if err != nil {
		t.Fatal(err)
	}
	info, known := modelSupportWithClient(context.Background(), client, "test-model", providerAPI+"/responses")
	if !known || info.ImageSupport != llm.InputSupportSupported || info.PDFSupport != llm.InputSupportUnsupported {
		t.Fatalf("catalog fallback = %#v, %v", info, known)
	}
}

func TestModelCostEstimatorFallsBackToCatalog(t *testing.T) {
	service := newCatalogService(t, catalogFixture("https://catalog.example.com/v1", "test-model", modelWithFullMetadata))
	useModelsDevService(t, service)

	estimator := modelCostEstimator("https://catalog.example.com/v1/responses", "test-model")
	if estimator == nil {
		t.Fatal("modelCostEstimator() = nil")
	}
	cost, ok := estimator(llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok || cost != 0.75 {
		t.Fatalf("estimated cost = %v, %v, want 0.75, true", cost, ok)
	}
	if modelCostEstimator("https://unknown.example.com/v1", "test-model") != nil {
		t.Fatal("estimator matched an unknown provider")
	}
}

func TestModelDetailsForEndpoint(t *testing.T) {
	body := `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": "https://catalog.example.com/v1",
			"models": {
				"test-model": ` + modelWithFullMetadata + `,
				"limited-model": {
					"name": "Limited Model",
					"tool_call": false,
					"status": "deprecated",
					"modalities": {"input": ["text", "pdf"], "output": ["text"]},
					"limit": {"context": 128000},
					"cost": {"input": 1, "output": 2}
				}
			}
		}
	}`
	service := newCatalogService(t, body)
	useModelsDevService(t, service)

	details := modelDetailsForEndpoint("https://catalog.example.com/v1/responses", []string{"test-model", "limited-model", "missing"})
	full := details["test-model"]
	if len(full.Modalities) != 2 || full.Modalities[0] != "text" || full.Modalities[1] != "image" {
		t.Errorf("full modalities = %v", full.Modalities)
	}
	limited := details["limited-model"]
	if len(limited.Modalities) != 2 || limited.Modalities[0] != "text" || limited.Modalities[1] != "pdf" {
		t.Errorf("limited modalities = %v", limited.Modalities)
	}
	if _, ok := details["missing"]; ok {
		t.Error("missing model received details")
	}
}

func TestAvailableModelsUsesModelsDevFallback(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(provider.Close)
	providerAPI := provider.URL + "/v1"
	body := `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": ` + jsonString(providerAPI) + `,
			"models": {
				"model-b": ` + modelWithFullMetadata + `,
				"model-a": ` + modelWithFullMetadata + `,
				"image-model": {
					"name": "Image Model",
					"modalities": {"input": ["text"], "output": ["image"]}
				}
			}
		}
	}`
	useModelsDevService(t, newCatalogService(t, body))
	t.Setenv("LLM_ENDPOINT", providerAPI+"/responses")
	t.Setenv("LLM_KEY", "test-key")

	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	models, selected, fallback := availableModels(request, database, "selected-model")
	if !fallback {
		t.Fatal("availableModels() fallback = false, want true")
	}
	if selected != "selected-model" {
		t.Fatalf("selected model = %q", selected)
	}
	want := []string{"selected-model", "model-a", "model-b"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for index := range want {
		if models[index] != want[index] {
			t.Fatalf("models = %v, want %v", models, want)
		}
	}
}

func TestAvailableModelsProviderListWins(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("request path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"provider-model"}]}`)
	}))
	t.Cleanup(provider.Close)
	providerAPI := provider.URL + "/v1"
	body := `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": ` + jsonString(providerAPI) + `,
			"models": {"model-a": ` + modelWithFullMetadata + `}
		}
	}`
	useModelsDevService(t, newCatalogService(t, body))
	t.Setenv("LLM_ENDPOINT", providerAPI+"/responses")
	t.Setenv("LLM_KEY", "test-key")

	database := openTestDatabase(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	models, selected, fallback := availableModels(request, database, "provider-model")
	if fallback {
		t.Fatal("availableModels() used fallback despite provider list")
	}
	if selected != "provider-model" || len(models) != 1 || models[0] != "provider-model" {
		t.Fatalf("models = %v, selected = %q", models, selected)
	}
}

func TestHomeShowsModelsDevFallbackNote(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(provider.Close)
	providerAPI := provider.URL + "/v1"
	body := `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": ` + jsonString(providerAPI) + `,
			"models": {"model-a": ` + modelWithFullMetadata + `}
		}
	}`
	useModelsDevService(t, newCatalogService(t, body))
	t.Setenv("LLM_ENDPOINT", providerAPI+"/responses")
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "selected-model")

	database := openTestDatabase(t)
	response := httptest.NewRecorder()
	homeHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(
		response,
		httptest.NewRequest(http.MethodGet, "/", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("home status = %d", response.Code)
	}
	bodyText := response.Body.String()
	if !strings.Contains(bodyText, "Using models.dev fallback") {
		t.Fatal("home page missing models.dev fallback note")
	}
	if !strings.Contains(bodyText, `value="model-a"`) || !strings.Contains(bodyText, `value="selected-model"`) {
		t.Fatal("home page missing fallback model options")
	}
	if !strings.Contains(bodyText, `aria-label="Image"`) {
		t.Fatal("home page missing model modality icon")
	}
}

func TestSettingsShowsModelModalities(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(provider.Close)
	providerAPI := provider.URL + "/v1"
	body := `{
		"local": {
			"id": "local",
			"name": "Local Provider",
			"api": ` + jsonString(providerAPI) + `,
			"models": {"model-a": ` + modelWithFullMetadata + `}
		}
	}`
	useModelsDevService(t, newCatalogService(t, body))
	t.Setenv("LLM_ENDPOINT", providerAPI+"/responses")
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "model-a")

	database := openTestDatabase(t)
	seedCurrentChat(t, database, 8)
	response := httptest.NewRecorder()
	settingsHandler(database, newTestToolRegistry(t))(response, httptest.NewRequest(http.MethodGet, "/settings?chat=8", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d", response.Code)
	}
	bodyText := response.Body.String()
	for _, want := range []string{
		`id="default-model"`,
		`class="settings-model-picker"`,
		`aria-label="Image"`,
		`Using models.dev fallback`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	if strings.Contains(bodyText, "per 1M") {
		t.Error("settings page still renders price text")
	}
}

func TestModalityKind(t *testing.T) {
	tests := map[string]string{
		"text":            "text",
		"image/png":       "image",
		"application/pdf": "pdf",
		"document":        "pdf",
		"audio/mpeg":      "audio",
		"video/mp4":       "video",
		"unknown":         "",
	}
	for input, want := range tests {
		if got := modalityKind(input); got != want {
			t.Errorf("modalityKind(%q) = %q, want %q", input, got, want)
		}
	}
}
