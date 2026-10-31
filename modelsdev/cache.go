package modelsdev

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// ErrCacheEmpty reports that no cached catalog exists.
var ErrCacheEmpty = errors.New("modelsdev: cache is empty")

// CacheEntry is one persisted catalog payload.
type CacheEntry struct {
	Payload   []byte
	ETag      string
	FetchedAt time.Time
}

// Cache persists the public models.dev catalog.
type Cache interface {
	Load(context.Context) (CacheEntry, error)
	Save(context.Context, CacheEntry) error
}

// MemoryCache is an in-memory Cache useful for tests and non-persistent use.
type MemoryCache struct {
	mu    sync.Mutex
	entry CacheEntry
	has   bool
}

// Load returns the in-memory entry.
func (c *MemoryCache) Load(context.Context) (CacheEntry, error) {
	if c == nil {
		return CacheEntry{}, ErrCacheEmpty
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.has {
		return CacheEntry{}, ErrCacheEmpty
	}
	entry := c.entry
	entry.Payload = append([]byte(nil), c.entry.Payload...)
	return entry, nil
}

// Save stores a copy of the entry.
func (c *MemoryCache) Save(_ context.Context, entry CacheEntry) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.Payload = append([]byte(nil), entry.Payload...)
	c.entry = entry
	c.has = true
	return nil
}

// SQLCache stores the catalog in the app's SQLite database. The
// models_dev_cache table is created by the application schema migration.
type SQLCache struct {
	DB *sql.DB
}

// NewSQLCache returns a cache backed by database.
func NewSQLCache(database *sql.DB) *SQLCache {
	return &SQLCache{DB: database}
}

// Load reads the single cached catalog row.
func (c *SQLCache) Load(ctx context.Context) (CacheEntry, error) {
	if c == nil || c.DB == nil {
		return CacheEntry{}, ErrCacheEmpty
	}
	var entry CacheEntry
	var fetchedAt int64
	err := c.DB.QueryRowContext(ctx, `
		SELECT etag, fetched_at, payload FROM models_dev_cache WHERE id = 1
	`).Scan(&entry.ETag, &fetchedAt, &entry.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, ErrCacheEmpty
	}
	if err != nil {
		return CacheEntry{}, err
	}
	if fetchedAt > 0 {
		entry.FetchedAt = time.Unix(fetchedAt, 0)
	}
	entry.Payload = append([]byte(nil), entry.Payload...)
	return entry, nil
}

// Save replaces the single cached catalog row.
func (c *SQLCache) Save(ctx context.Context, entry CacheEntry) error {
	if c == nil || c.DB == nil {
		return nil
	}
	var fetchedAt int64
	if !entry.FetchedAt.IsZero() {
		fetchedAt = entry.FetchedAt.Unix()
	}
	_, err := c.DB.ExecContext(ctx, `
		INSERT INTO models_dev_cache (id, etag, fetched_at, payload)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			etag = excluded.etag,
			fetched_at = excluded.fetched_at,
			payload = excluded.payload
	`, entry.ETag, fetchedAt, entry.Payload)
	return err
}
