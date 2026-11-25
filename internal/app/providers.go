package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
)

const (
	envProviderName     = "Environment"
	legacyProviderName  = "Saved connection"
	providerListTimeout = 8 * time.Second
)

// providerEntry is one resolvable model provider. ID is the stored provider
// ID, kritui_db.EnvProviderID for process environment, or 0 for legacy single-provider
// storage that has not been migrated yet.
type providerEntry struct {
	ID       int64
	Name     string
	Endpoint string
	APIKey   string
	ReadOnly bool
}

// modelOption is one selectable provider/model pair. Value is the form value
// passed between templates and handlers; Model is the provider's model ID.
type modelOption struct {
	Value        string
	Model        string
	ProviderID   int64
	ProviderName string
}

type modelCatalog struct {
	Providers []templates.ProviderCard
	Entries   []providerEntry
	Options   []modelOption
	Details   map[string]templates.ModelDetail
	Fallback  bool
}

// loadProviderEntries returns stored providers in display order, plus the
// synthetic environment provider when configured. A pre-migration account with
// only the legacy settings connection is exposed as provider 0.
func loadProviderEntries(ctx context.Context, database *sql.DB) ([]providerEntry, error) {
	configs, err := kritui_db.ListProviderConfigs(ctx, database)
	if err != nil {
		return nil, err
	}
	entries := make([]providerEntry, 0, len(configs)+2)
	for _, config := range configs {
		entries = append(entries, providerEntry{
			ID:       config.ID,
			Name:     config.Name,
			Endpoint: config.Endpoint,
			APIKey:   config.APIKey,
		})
	}
	if llmEnvironmentConfigured() {
		entries = append(entries, providerEntry{
			ID:       kritui_db.EnvProviderID,
			Name:     envProviderName,
			Endpoint: strings.TrimSpace(os.Getenv("LLM_ENDPOINT")),
			APIKey:   strings.TrimSpace(os.Getenv("LLM_KEY")),
			ReadOnly: true,
		})
	}
	if len(entries) == 0 {
		config, err := effectiveLLMConfig(ctx, database)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Endpoint) != "" {
			entries = append(entries, providerEntry{
				ID:       0,
				Name:     legacyProviderName,
				Endpoint: config.Endpoint,
				APIKey:   config.APIKey,
				ReadOnly: true,
			})
		}
	}
	return entries, nil
}

func providerCards(entries []providerEntry) []templates.ProviderCard {
	cards := make([]templates.ProviderCard, 0, len(entries))
	for _, entry := range entries {
		cards = append(cards, templates.ProviderCard{
			ID:               entry.ID,
			Name:             entry.Name,
			Endpoint:         entry.Endpoint,
			APIKeyConfigured: strings.TrimSpace(entry.APIKey) != "",
			ReadOnly:         entry.ReadOnly,
		})
	}
	return cards
}

// defaultProviderID chooses the account default, then the environment provider
// when no stored provider exists, then the first provider in display order.
func defaultProviderID(ctx context.Context, database *sql.DB, entries []providerEntry) int64 {
	configured, err := kritui_db.GetDefaultProviderID(ctx, database)
	if err != nil {
		log.Printf("get default provider ID: %v", err)
	}
	if configured > 0 {
		for _, entry := range entries {
			if entry.ID == configured {
				return configured
			}
		}
	}
	for _, entry := range entries {
		if entry.ID > 0 {
			return entry.ID
		}
	}
	for _, entry := range entries {
		if entry.ID == kritui_db.EnvProviderID {
			return kritui_db.EnvProviderID
		}
	}
	if len(entries) > 0 {
		return entries[0].ID
	}
	return 0
}

func providerEntryByID(entries []providerEntry, providerID int64) (providerEntry, bool) {
	for _, entry := range entries {
		if entry.ID == providerID {
			return entry, true
		}
	}
	return providerEntry{}, false
}

// modelSelectionValue encodes a provider and model for form values. Provider 0
// and the environment provider are encoded too so a picker always carries an
// explicit provider.
func modelSelectionValue(providerID int64, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strconv.FormatInt(providerID, 10) + "/" + model
}

// parseModelSelection decodes provider/model form values. Raw model IDs from
// older clients and manual inputs report provider 0.
func parseModelSelection(value string) (int64, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, ""
	}
	prefix, model, ok := strings.Cut(value, "/")
	if !ok || model == "" {
		return 0, value
	}
	providerID, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, value
	}
	return providerID, model
}

// selectedProvider resolves a submitted provider ID against the current list.
// Unknown IDs fall back to the account default so deleting a provider never
// leaves an unusable selection.
func selectedProvider(entries []providerEntry, providerID, defaultID int64) providerEntry {
	if entry, ok := providerEntryByID(entries, providerID); ok {
		return entry
	}
	if entry, ok := providerEntryByID(entries, defaultID); ok {
		return entry
	}
	if len(entries) > 0 {
		return entries[0]
	}
	return providerEntry{}
}

// providerConfig returns the credentials for one resolved provider.
func providerConfig(entry providerEntry) kritui_db.LLMConfig {
	return kritui_db.LLMConfig{Endpoint: entry.Endpoint, APIKey: entry.APIKey}
}

// buildModelCatalog lists models from every configured provider. Provider
// failures fall back to the models.dev catalog and never block the page.
func buildModelCatalog(ctx context.Context, database *sql.DB, sessionID string, providerID int64, model string) (modelCatalog, error) {
	entries, err := loadProviderEntries(ctx, database)
	if err != nil {
		return modelCatalog{}, err
	}
	catalog := modelCatalog{
		Providers: providerCards(entries),
		Entries:   entries,
	}
	if len(entries) == 0 {
		return catalog, nil
	}
	defaultID := defaultProviderID(ctx, database, entries)
	selected := selectedProvider(entries, providerID, defaultID)
	model = strings.TrimSpace(model)

	type providerModels struct {
		models   []string
		fallback bool
	}
	results := make([]providerModels, len(entries))
	var wait sync.WaitGroup
	for index, entry := range entries {
		wait.Add(1)
		go func(index int, entry providerEntry) {
			defer wait.Done()
			models, fallback := listProviderModels(ctx, entry, sessionID)
			results[index] = providerModels{models: models, fallback: fallback}
		}(index, entry)
	}
	wait.Wait()

	for index, entry := range entries {
		result := results[index]
		catalog.Fallback = catalog.Fallback || result.fallback
		for _, modelID := range result.models {
			value := modelSelectionValue(entry.ID, modelID)
			catalog.Options = append(catalog.Options, modelOption{
				Value:        value,
				Model:        modelID,
				ProviderID:   entry.ID,
				ProviderName: entry.Name,
			})
		}
	}

	if model != "" {
		catalog.Options = mergeSelectedModelOption(catalog.Options, selected, model)
	}

	catalog.Details = make(map[string]templates.ModelDetail)
	for index, entry := range entries {
		modelIDs := make([]string, 0, len(results[index].models))
		modelIDs = append(modelIDs, results[index].models...)
		if entry.ID == selected.ID && model != "" {
			modelIDs = append(modelIDs, model)
		}
		details := modelDetailsForEndpoint(entry.Endpoint, modelIDs)
		for modelID, detail := range details {
			catalog.Details[modelSelectionValue(entry.ID, modelID)] = detail
		}
	}
	if len(catalog.Details) == 0 {
		catalog.Details = nil
	}
	return catalog, nil
}

func mergeSelectedModelOption(options []modelOption, provider providerEntry, model string) []modelOption {
	value := modelSelectionValue(provider.ID, model)
	for _, option := range options {
		if option.Value == value {
			return options
		}
	}
	return append([]modelOption{{
		Value:        value,
		Model:        model,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
	}}, options...)
}

func listProviderModels(ctx context.Context, entry providerEntry, sessionID string) ([]string, bool) {
	if strings.TrimSpace(entry.Endpoint) == "" {
		return nil, false
	}
	listContext, cancel := context.WithTimeout(ctx, providerListTimeout)
	defer cancel()
	client, err := llm.New(entry.APIKey, "models", entry.Endpoint, llm.ClientOptions{
		SessionID:     sessionID,
		ModelsTimeout: providerListTimeout,
	})
	if err == nil {
		infos, listErr := client.ModelInfos(listContext)
		if listErr == nil && len(infos) > 0 {
			models := make([]string, 0, len(infos))
			for _, info := range infos {
				models = append(models, info.ID)
				rememberContextWindow(entry, info.ID, info.ContextWindow)
			}
			return models, false
		}
		if listErr != nil {
			log.Printf("list models for provider %q: %v", entry.Name, listErr)
		}
	} else {
		log.Printf("configure provider %q model list: %v", entry.Name, err)
	}
	if modelsDevService != nil {
		if models, ok := modelsDevService.Models(entry.Endpoint); ok && len(models) > 0 {
			return models, true
		}
	}
	return nil, false
}

// modelCatalogData copies catalog collections into template data fields.
func modelCatalogData(catalog modelCatalog, selectedProviderID int64, selectedModel string) ([]string, map[string]string, map[string]string, map[string]templates.ModelDetail, string) {
	values := make([]string, 0, len(catalog.Options))
	names := make(map[string]string, len(catalog.Options))
	providers := make(map[string]string, len(catalog.Options))
	for _, option := range catalog.Options {
		values = append(values, option.Value)
		names[option.Value] = option.Model
		providers[option.Value] = option.ProviderName
	}
	selectedValue := modelSelectionValue(selectedProviderID, selectedModel)
	return values, names, providers, catalog.Details, selectedValue
}

// providerEndpoint resolves the endpoint for a submitted provider/model pair
// without failing settings validation when a provider has just been removed.
func providerEndpoint(entries []providerEntry, providerID, defaultID int64) string {
	return selectedProvider(entries, providerID, defaultID).Endpoint
}

// resolveProviderConfig loads credentials for a completion request. Provider 0
// uses the account default; an empty account falls back to the legacy settings
// connection so pre-migration databases still work.
func resolveProviderConfig(ctx context.Context, database *sql.DB, providerID int64) (providerEntry, error) {
	entries, err := loadProviderEntries(ctx, database)
	if err != nil {
		return providerEntry{}, err
	}
	defaultID := defaultProviderID(ctx, database, entries)
	entry := selectedProvider(entries, providerID, defaultID)
	if strings.TrimSpace(entry.Endpoint) == "" {
		config, err := effectiveLLMConfig(ctx, database)
		if err != nil {
			return providerEntry{}, err
		}
		if strings.TrimSpace(config.Endpoint) == "" {
			return providerEntry{}, fmt.Errorf("no model provider is configured")
		}
		return providerEntry{ID: 0, Name: legacyProviderName, Endpoint: config.Endpoint, APIKey: config.APIKey}, nil
	}
	return entry, nil
}

// providerDraftsFromForm reads repeated provider fields in display order. Rows
// with no id and no values are ignored so an untouched empty template row does
// not create a provider.
func providerDraftsFromForm(r *http.Request, entries []providerEntry) ([]kritui_db.ProviderDraft, error) {
	ids, names := r.Form["provider_id"], r.Form["provider_name"]
	endpoints, keys := r.Form["provider_endpoint"], r.Form["provider_api_key"]
	if len(names) != len(endpoints) || len(names) != len(keys) || len(ids) != len(names) {
		return nil, fmt.Errorf("provider form is incomplete")
	}
	clear := make(map[string]struct{})
	for _, value := range r.Form["provider_clear_key"] {
		clear[strings.TrimSpace(value)] = struct{}{}
	}
	drafts := make([]kritui_db.ProviderDraft, 0, len(names))
	for index := range names {
		idValue := strings.TrimSpace(ids[index])
		name := strings.TrimSpace(names[index])
		endpoint := strings.TrimSpace(endpoints[index])
		key := strings.TrimSpace(keys[index])
		var id int64
		if idValue != "" {
			parsed, err := strconv.ParseInt(idValue, 10, 64)
			if err != nil || parsed < 0 {
				return nil, fmt.Errorf("provider %d has an invalid id", index+1)
			}
			id = parsed
		}
		if id == 0 && name == "" && endpoint == "" && key == "" {
			continue
		}
		draft := kritui_db.ProviderDraft{ID: id, Name: name, Endpoint: endpoint}
		if id > 0 && hasStringKey(clear, idValue) && key != "" {
			return nil, fmt.Errorf("provider %d cannot replace and clear its API key at the same time", index+1)
		}
		switch {
		case id > 0 && hasStringKey(clear, idValue):
			draft.APIKeyChange = kritui_db.LLMClearAPIKey
		case key != "":
			draft.APIKeyChange = kritui_db.LLMReplaceAPIKey
			draft.APIKeyValue = key
		default:
			draft.APIKeyChange = kritui_db.LLMKeepAPIKey
		}
		drafts = append(drafts, draft)
	}
	return drafts, nil
}

func hasStringKey(values map[string]struct{}, key string) bool {
	_, ok := values[key]
	return ok
}

// selectionProviderID resolves an explicit provider field submitted next to a
// raw model input, then falls back to the parsed selection and account default.
func selectionProviderID(entries []providerEntry, providerID, defaultID int64) int64 {
	return selectedProvider(entries, providerID, defaultID).ID
}

// ensureCatalogOption adds a selected model that a provider's live list does
// not currently report, so saved settings never render an unchecked selection.
func ensureCatalogOption(catalog *modelCatalog, entries []providerEntry, providerID, defaultID int64, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	provider := selectedProvider(entries, providerID, defaultID)
	value := modelSelectionValue(provider.ID, model)
	for _, option := range catalog.Options {
		if option.Value == value {
			return
		}
	}
	catalog.Options = append(catalog.Options, modelOption{
		Value:        value,
		Model:        model,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
	})
	if catalog.Details == nil {
		catalog.Details = make(map[string]templates.ModelDetail)
	}
	if detail, ok := modelDetailsForEndpoint(provider.Endpoint, []string{model})[model]; ok {
		catalog.Details[value] = detail
	}
}
