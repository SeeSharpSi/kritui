package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultTTL           = 6 * time.Hour
	defaultFetchTimeout  = 20 * time.Second
	refreshTimeout       = 30 * time.Second
	refreshRetryInterval = 5 * time.Minute
	maxCatalogSize       = 16 << 20
)

// Options configures a Service.
type Options struct {
	URL        string
	HTTPClient *http.Client
	TTL        time.Duration
	Now        func() time.Time
	Logger     *log.Logger
}

// Service fetches, caches, and resolves the public models.dev catalog.
type Service struct {
	cache  Cache
	url    string
	client *http.Client
	ttl    time.Duration
	now    func() time.Time
	logger *log.Logger

	refreshMu  sync.Mutex
	startOnce  sync.Once
	refreshing atomic.Bool

	mu                 sync.RWMutex
	providers          map[string]Provider
	payload            []byte
	etag               string
	fetchedAt          time.Time
	loaded             bool
	lastRefreshAttempt time.Time
}

// NewService creates a catalog service. A nil cache keeps the catalog only in
// memory.
func NewService(cache Cache, options Options) (*Service, error) {
	rawURL := strings.TrimSpace(options.URL)
	if rawURL == "" {
		rawURL = DefaultURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("modelsdev: catalog URL must be an absolute HTTP or HTTPS URL")
	}
	ttl := options.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		cache:  cache,
		url:    parsed.String(),
		client: client,
		ttl:    ttl,
		now:    now,
		logger: logger,
	}, nil
}

// Load restores the cached catalog if one exists.
func (s *Service) Load(ctx context.Context) error {
	if s == nil || s.cache == nil {
		return nil
	}
	entry, err := s.cache.Load(ctx)
	if errors.Is(err, ErrCacheEmpty) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("modelsdev: load cache: %w", err)
	}
	providers, err := parseProviders(entry.Payload)
	if err != nil {
		return fmt.Errorf("modelsdev: parse cached catalog: %w", err)
	}
	s.install(providers, entry.Payload, entry.ETag, entry.FetchedAt)
	return nil
}

// Start loads the cache and launches one background refresh when the catalog
// is missing or stale. It never blocks on the network.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if !s.isLoaded() {
			if err := s.Load(ctx); err != nil {
				s.logger.Printf("%v", err)
			}
		}
		s.refreshIfStale()
	})
}

// Refresh fetches the catalog conditionally using its stored ETag.
func (s *Service) Refresh(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	etag, payload := s.currentPayload()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("modelsdev: create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "kritui/1.0")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("modelsdev: fetch catalog: %w", err)
	}
	defer response.Body.Close()

	fetchedAt := s.now().UTC()
	if response.StatusCode == http.StatusNotModified {
		if len(payload) == 0 {
			return errors.New("modelsdev: catalog returned not modified without a cached payload")
		}
		s.setFetchedAt(fetchedAt)
		if s.cache != nil {
			if err := s.cache.Save(ctx, CacheEntry{Payload: payload, ETag: etag, FetchedAt: fetchedAt}); err != nil {
				return fmt.Errorf("modelsdev: update cache: %w", err)
			}
		}
		return nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("modelsdev: fetch catalog: HTTP %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogSize+1))
	if err != nil {
		return fmt.Errorf("modelsdev: read catalog: %w", err)
	}
	if len(body) > maxCatalogSize {
		return fmt.Errorf("modelsdev: catalog exceeds %d bytes", maxCatalogSize)
	}
	providers, err := parseProviders(body)
	if err != nil {
		return fmt.Errorf("modelsdev: parse catalog: %w", err)
	}
	newETag := response.Header.Get("ETag")
	s.install(providers, body, newETag, fetchedAt)
	if s.cache != nil {
		if err := s.cache.Save(ctx, CacheEntry{Payload: body, ETag: newETag, FetchedAt: fetchedAt}); err != nil {
			return fmt.Errorf("modelsdev: save cache: %w", err)
		}
	}
	s.logger.Printf("models.dev: refreshed %d providers", len(providers))
	return nil
}

// Stale reports whether the catalog is absent or past its refresh interval.
func (s *Service) Stale() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.staleLocked()
}

func (s *Service) staleLocked() bool {
	if len(s.providers) == 0 || s.fetchedAt.IsZero() {
		return true
	}
	return s.now().UTC().Sub(s.fetchedAt) >= s.ttl
}

// refreshIfStale starts one best-effort background refresh. Failed attempts are
// throttled so model lookups cannot hammer the catalog during an outage.
func (s *Service) refreshIfStale() {
	if s == nil {
		return
	}
	s.mu.Lock()
	stale := s.staleLocked()
	now := s.now().UTC()
	if !stale || (!s.lastRefreshAttempt.IsZero() && now.Sub(s.lastRefreshAttempt) < refreshRetryInterval) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	s.lastRefreshAttempt = now
	s.mu.Unlock()
	go func() {
		defer s.refreshing.Store(false)
		refreshContext, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		if err := s.Refresh(refreshContext); err != nil {
			s.logger.Printf("models.dev: refresh failed: %v", err)
		}
	}()
}

// Models returns the catalog model IDs for the provider matched by endpoint.
// Non-text-output models are omitted. The boolean is false when the endpoint
// has no catalog provider or no usable models.
func (s *Service) Models(endpoint string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	target := NormalizeEndpoint(endpoint)
	if target == "" {
		return nil, false
	}
	s.refreshIfStale()
	s.mu.RLock()
	providers := s.providers
	s.mu.RUnlock()
	provider, ok := matchProvider(providers, target)
	if !ok {
		return nil, false
	}
	models := make([]string, 0, len(provider.Models))
	for _, id := range sortedModelIDs(provider) {
		if !provider.Models[id].hasTextOutput() {
			continue
		}
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, false
	}
	return models, true
}

// Lookup resolves a configured completion endpoint and model ID to one
// provider model. It returns false rather than guessing when either the
// provider or model cannot be matched.
func (s *Service) Lookup(endpoint, modelID string) (ProviderModel, bool) {
	if s == nil {
		return ProviderModel{}, false
	}
	target := NormalizeEndpoint(endpoint)
	modelID = strings.TrimSpace(modelID)
	if target == "" || modelID == "" {
		return ProviderModel{}, false
	}
	s.refreshIfStale()
	s.mu.RLock()
	providers := s.providers
	s.mu.RUnlock()
	if len(providers) == 0 {
		return ProviderModel{}, false
	}

	provider, ok := matchProvider(providers, target)
	if !ok {
		return ProviderModel{}, false
	}
	modelKey, model, ok := resolveModel(provider, modelID)
	if !ok {
		return ProviderModel{}, false
	}
	return ProviderModel{
		ProviderID: provider.ID,
		Provider:   provider,
		ModelID:    modelKey,
		Model:      model,
	}, true
}

func (s *Service) isLoaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

func (s *Service) currentPayload() (string, []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.etag, append([]byte(nil), s.payload...)
}

func (s *Service) install(providers map[string]Provider, payload []byte, etag string, fetchedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers = providers
	s.payload = append([]byte(nil), payload...)
	s.etag = etag
	s.fetchedAt = fetchedAt
	s.loaded = true
}

func (s *Service) setFetchedAt(fetchedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchedAt = fetchedAt
	s.loaded = true
}

func parseProviders(payload []byte) (map[string]Provider, error) {
	var envelope struct {
		Providers json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	var providers map[string]Provider
	if len(envelope.Providers) > 0 {
		if err := json.Unmarshal(envelope.Providers, &providers); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(payload, &providers); err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, errors.New("catalog contains no providers")
	}
	for providerID, provider := range providers {
		if provider.ID == "" {
			provider.ID = providerID
		}
		if provider.Models == nil {
			provider.Models = map[string]Model{}
		}
		for modelID, model := range provider.Models {
			if model.ID == "" {
				model.ID = modelID
			}
			provider.Models[modelID] = model
		}
		providers[providerID] = provider
	}
	return providers, nil
}

func matchProvider(providers map[string]Provider, target string) (Provider, bool) {
	exact := matchProviderVariant(providers, target, false)
	if exact != nil {
		return *exact, true
	}
	stripped := matchProviderVariant(providers, stripV1(target), true)
	if stripped != nil {
		return *stripped, true
	}
	return Provider{}, false
}

func matchProviderVariant(providers map[string]Provider, target string, strip bool) *Provider {
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var best *Provider
	bestLength := -1
	for _, id := range ids {
		provider := providers[id]
		api := NormalizeEndpoint(provider.API)
		if api == "" {
			continue
		}
		if strip {
			api = stripV1(api)
		}
		if api != target {
			continue
		}
		length := endpointPathLength(api)
		if length > bestLength || (length == bestLength && (best == nil || id < best.ID)) {
			candidate := provider
			best = &candidate
			bestLength = length
		}
	}
	return best
}

func resolveModel(provider Provider, modelID string) (string, Model, bool) {
	if model, ok := provider.Models[modelID]; ok {
		return modelID, model, true
	}
	ids := sortedModelIDs(provider)
	for _, id := range ids {
		if provider.Models[id].CanonicalModelID == modelID {
			return id, provider.Models[id], true
		}
	}
	var matchedID string
	matches := 0
	for _, id := range ids {
		canonical := provider.Models[id].CanonicalModelID
		if canonical == "" {
			continue
		}
		segment := canonical
		if index := strings.LastIndex(segment, "/"); index >= 0 {
			segment = segment[index+1:]
		}
		if segment == modelID {
			matchedID = id
			matches++
		}
	}
	if matches == 1 {
		return matchedID, provider.Models[matchedID], true
	}
	return "", Model{}, false
}
