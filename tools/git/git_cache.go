package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gitMaxRepositoriesPerUser       = 8
	gitMaxCacheBytes          int64 = 2 * 1024 * 1024 * 1024
	gitCacheLastUsedFile            = ".kritui-last-used"
	gitCacheTempPrefix              = "."
)

// gitSyncJob describes one clone or fetch. Directory is always inside the
// account cache. URL never contains credentials.
type gitSyncJob struct {
	Directory string
	URL       string
	Username  string
	Token     string
	Clone     bool
}

type gitRuntime struct {
	rootMu sync.Mutex
	root   string

	maxEntries        int
	maxCacheBytes     int64
	maxRepositorySize int64
	minFreeBytes      int64

	now            func() time.Time
	syncRepository func(context.Context, gitSyncJob) error
}

type gitRepository struct {
	directory string
	address   string
	secrets   []string
}

type cacheEntry struct {
	path     string
	size     int64
	lastUsed time.Time
}

type cacheEntryLock struct {
	lock sync.RWMutex
	refs int
}

var (
	cacheEntryLocksMu sync.Mutex
	cacheEntryLocks   = make(map[string]*cacheEntryLock)
	cacheUserLocksMu  sync.Mutex
	cacheUserLocks    = make(map[string]*sync.Mutex)
)

func newGitRuntime() *gitRuntime {
	return &gitRuntime{
		maxEntries:        gitMaxRepositoriesPerUser,
		maxCacheBytes:     gitMaxCacheBytes,
		maxRepositorySize: gitRepositoryMaxSize,
		minFreeBytes:      gitFreeSpaceReserve,
		now:               time.Now,
		syncRepository:    runDefaultGitSync,
	}
}

func (r *gitRuntime) cacheRoot() (string, error) {
	r.rootMu.Lock()
	defer r.rootMu.Unlock()
	if r.root != "" {
		return r.root, nil
	}
	root := strings.TrimSpace(os.Getenv("KRITUI_GIT_CACHE_DIR"))
	if root == "" {
		root = "git-cache"
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("git: resolve cache directory: %w", err)
	}
	r.root = filepath.Clean(absolute)
	return r.root, nil
}

func (r *gitRuntime) accountDirectory(userID int64) (string, error) {
	root, err := r.cacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("git: create cache directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("git: protect cache directory: %w", err)
	}
	directory := filepath.Join(root, strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("git: create account cache directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("git: protect account cache directory: %w", err)
	}
	return directory, nil
}

func cachePathFor(accountDirectory, address string) string {
	sum := sha256.Sum256([]byte(address))
	return filepath.Join(accountDirectory, hex.EncodeToString(sum[:8]))
}

func (r *gitRuntime) withRepository(ctx context.Context, rawURL string, operation func(*gitRepository) (string, error)) (string, error) {
	session, ok := ctx.Value(gitSessionContextKey{}).(*GitSession)
	if !ok || session == nil {
		return "", errors.New("git: request session is unavailable")
	}

	repository, err := r.repository(ctx, session, rawURL)
	if err != nil {
		return "", err
	}

	entry := cacheEntryLockFor(repository.directory)
	entry.lock.RLock()
	defer entry.lock.RUnlock()

	result, operationErr := operation(repository)
	if operationErr != nil {
		return "", redactGitError(operationErr, repository.secrets)
	}
	result = redactGitSecrets(result, repository.secrets)
	if len(result) > gitMaxResultBytes {
		return "", errors.New("git: result exceeded output limit")
	}
	return result, nil
}

func (r *gitRuntime) repository(ctx context.Context, session *GitSession, rawURL string) (*gitRepository, error) {
	address, username, token, err := session.repositoryTarget(rawURL)
	if err != nil {
		return nil, err
	}
	accountDirectory, err := r.accountDirectory(session.userID)
	if err != nil {
		return nil, err
	}
	directory := cachePathFor(accountDirectory, address)
	redactions := gitRedactionValues(username, token)

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil, errors.New("git: request session is closed")
	}
	if repository := session.repositories[directory]; repository != nil {
		r.touchEntry(directory)
		return repository, nil
	}

	unlockUser := lockUserCache(accountDirectory)
	defer unlockUser()
	entry := cacheEntryLockFor(directory)
	entry.lock.Lock()
	defer entry.lock.Unlock()

	if session.closed {
		return nil, errors.New("git: request session is closed")
	}
	if err := r.ensureCacheEntry(ctx, directory, address, username, token, accountDirectory); err != nil {
		return nil, redactGitError(err, redactions)
	}

	retainCacheEntry(directory)
	repository := &gitRepository{
		directory: directory,
		address:   address,
		secrets:   redactions,
	}
	session.repositories[directory] = repository
	r.touchEntry(directory)
	return repository, nil
}

func (r *gitRuntime) ensureCacheEntry(ctx context.Context, directory, address, username, token, accountDirectory string) error {
	exists := false
	info, err := os.Stat(directory)
	switch {
	case err == nil:
		if !info.IsDir() || !isValidMirror(directory) {
			if err := os.RemoveAll(directory); err != nil {
				return fmt.Errorf("git: remove corrupt cache entry: %w", err)
			}
		} else {
			exists = true
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return fmt.Errorf("git: inspect cache entry: %w", err)
	}

	if err := r.checkFreeDisk(accountDirectory); err != nil {
		return err
	}

	if exists {
		if err := r.syncRepository(ctx, gitSyncJob{
			Directory: directory,
			URL:       address,
			Username:  username,
			Token:     token,
			Clone:     false,
		}); err != nil {
			if ctx.Err() != nil {
				return err
			}
			if !isValidMirror(directory) {
				if removeErr := os.RemoveAll(directory); removeErr != nil {
					return errors.Join(err, fmt.Errorf("git: remove corrupt cache entry: %w", removeErr))
				}
				exists = false
			} else {
				return err
			}
		}
	}

	if !exists {
		if err := r.evictForNewEntry(accountDirectory, directory); err != nil {
			return err
		}
		if err := r.cloneRepository(ctx, directory, address, username, token, accountDirectory); err != nil {
			return err
		}
	}

	if err := r.checkRepositoryLimit(directory); err != nil {
		if !cacheEntryReferenced(directory) {
			_ = os.RemoveAll(directory)
		}
		return err
	}
	if err := r.enforceCacheLimits(accountDirectory, directory, !exists); err != nil {
		return err
	}
	r.touchEntry(directory)
	return nil
}

func (r *gitRuntime) cloneRepository(ctx context.Context, directory, address, username, token, accountDirectory string) (resultErr error) {
	temporary, err := os.MkdirTemp(accountDirectory, gitCacheTempPrefix+filepath.Base(directory)+"-tmp-")
	if err != nil {
		return fmt.Errorf("git: create temporary cache directory: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if err := os.RemoveAll(temporary); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("git: remove failed temporary clone: %w", err))
		}
	}()

	if err := r.syncRepository(ctx, gitSyncJob{
		Directory: temporary,
		URL:       address,
		Username:  username,
		Token:     token,
		Clone:     true,
	}); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		return fmt.Errorf("git: protect cache entry: %w", err)
	}
	if err := os.Rename(temporary, directory); err != nil {
		return fmt.Errorf("git: install cache entry: %w", err)
	}
	complete = true
	return nil
}

func runDefaultGitSync(ctx context.Context, job gitSyncJob) error {
	redactions := gitRedactionValues(job.Username, job.Token)
	if job.Clone {
		accountDirectory := filepath.Dir(job.Directory)
		return runGitCommand(ctx, gitCommandOptions{
			directory:    accountDirectory,
			resourceRoot: job.Directory,
			diskRoot:     accountDirectory,
			timeout:      gitCloneTimeout,
			username:     job.Username,
			token:        job.Token,
			secrets:      redactions,
			config:       gitCredentialArguments(job.Username, job.Token),
		}, discardGitOutput, "clone", "--mirror", "--", job.URL, job.Directory)
	}
	return runGitCommand(ctx, gitCommandOptions{
		directory:    job.Directory,
		resourceRoot: job.Directory,
		diskRoot:     job.Directory,
		timeout:      gitCloneTimeout,
		username:     job.Username,
		token:        job.Token,
		secrets:      redactions,
		config:       gitCredentialArguments(job.Username, job.Token),
	}, discardGitOutput, "fetch", "--prune")
}

func isValidMirror(directory string) bool {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return false
	}
	for _, name := range []string{"HEAD", "config", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			return false
		}
	}
	return true
}

func (r *gitRuntime) checkFreeDisk(directory string) error {
	available, err := availableDiskBytes(directory)
	if err != nil {
		return fmt.Errorf("git: check free disk space: %w", err)
	}
	if available < uint64(r.minFreeBytes) {
		return fmt.Errorf("git: insufficient free disk space: need at least %s, have %s",
			formatGitBytes(uint64(r.minFreeBytes)), formatGitBytes(available))
	}
	return nil
}

func (r *gitRuntime) checkRepositoryLimit(directory string) error {
	size, err := measureGitDirectory(directory, r.maxRepositorySize+1)
	if err != nil {
		return fmt.Errorf("git: measure cached repository: %w", err)
	}
	if size > r.maxRepositorySize {
		return fmt.Errorf("git: repository exceeds %s", formatGitBytes(uint64(r.maxRepositorySize)))
	}
	return nil
}

func (r *gitRuntime) evictForNewEntry(accountDirectory, exclude string) error {
	entries, total, err := r.scanCache(accountDirectory)
	if err != nil {
		return err
	}
	for len(entries) >= r.maxEntries {
		evicted, err := r.evictOldest(accountDirectory, exclude)
		if err != nil {
			return err
		}
		if !evicted {
			return fmt.Errorf("git: account git cache is full: all %d cached repositories are in use", len(entries))
		}
		entries, total, err = r.scanCache(accountDirectory)
		if err != nil {
			return err
		}
	}
	for total >= r.maxCacheBytes {
		evicted, err := r.evictOldest(accountDirectory, exclude)
		if err != nil {
			return err
		}
		if !evicted {
			return fmt.Errorf("git: account git cache exceeds %s and cached repositories are in use", formatGitBytes(uint64(r.maxCacheBytes)))
		}
		entries, total, err = r.scanCache(accountDirectory)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *gitRuntime) enforceCacheLimits(accountDirectory, keep string, newEntry bool) error {
	entries, total, err := r.scanCache(accountDirectory)
	if err != nil {
		return err
	}
	for len(entries) > r.maxEntries || total > r.maxCacheBytes {
		evicted, err := r.evictOldest(accountDirectory, keep)
		if err != nil {
			return err
		}
		if !evicted {
			if newEntry {
				_ = os.RemoveAll(keep)
				return fmt.Errorf("git: git cache entry exceeds the %s account cache limit; close other Git sessions or choose a smaller repository", formatGitBytes(uint64(r.maxCacheBytes)))
			}
			return fmt.Errorf("git: account git cache exceeds %s and cached repositories are in use", formatGitBytes(uint64(r.maxCacheBytes)))
		}
		entries, total, err = r.scanCache(accountDirectory)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *gitRuntime) evictOldest(accountDirectory, exclude string) (bool, error) {
	entries, _, err := r.scanCache(accountDirectory)
	if err != nil {
		return false, err
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].lastUsed.Equal(entries[right].lastUsed) {
			return entries[left].path < entries[right].path
		}
		return entries[left].lastUsed.Before(entries[right].lastUsed)
	})
	for _, entry := range entries {
		if entry.path == exclude {
			continue
		}
		if cacheEntryReferenced(entry.path) {
			continue
		}
		lock := cacheEntryLockFor(entry.path)
		if !lock.lock.TryLock() {
			continue
		}
		if cacheEntryReferenced(entry.path) {
			lock.lock.Unlock()
			continue
		}
		removeErr := os.RemoveAll(entry.path)
		lock.lock.Unlock()
		if removeErr != nil {
			return false, fmt.Errorf("git: evict cached repository: %w", removeErr)
		}
		return true, nil
	}
	return false, nil
}

func (r *gitRuntime) scanCache(accountDirectory string) ([]cacheEntry, int64, error) {
	directoryEntries, err := os.ReadDir(accountDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("git: list account cache: %w", err)
	}
	entries := make([]cacheEntry, 0, len(directoryEntries))
	var total int64
	for _, directoryEntry := range directoryEntries {
		if strings.HasPrefix(directoryEntry.Name(), gitCacheTempPrefix) || !directoryEntry.IsDir() {
			continue
		}
		entryPath := filepath.Join(accountDirectory, directoryEntry.Name())
		size, err := measureGitDirectory(entryPath, r.maxCacheBytes+1)
		if err != nil {
			return nil, 0, fmt.Errorf("git: measure cached repository: %w", err)
		}
		entries = append(entries, cacheEntry{
			path:     entryPath,
			size:     size,
			lastUsed: r.lastUsed(entryPath),
		})
		total += size
	}
	return entries, total, nil
}

func (r *gitRuntime) lastUsed(directory string) time.Time {
	info, err := os.Stat(filepath.Join(directory, gitCacheLastUsedFile))
	if err == nil {
		return info.ModTime()
	}
	info, err = os.Stat(directory)
	if err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

func (r *gitRuntime) touchEntry(directory string) {
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	marker := filepath.Join(directory, gitCacheLastUsedFile)
	if err := os.Chtimes(marker, now, now); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := os.WriteFile(marker, []byte(now.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
			return
		}
		_ = os.Chtimes(marker, now, now)
	}
}

func lockUserCache(directory string) func() {
	cacheUserLocksMu.Lock()
	lock := cacheUserLocks[directory]
	if lock == nil {
		lock = &sync.Mutex{}
		cacheUserLocks[directory] = lock
	}
	cacheUserLocksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func cacheEntryLockFor(directory string) *cacheEntryLock {
	directory = filepath.Clean(directory)
	cacheEntryLocksMu.Lock()
	defer cacheEntryLocksMu.Unlock()
	lock := cacheEntryLocks[directory]
	if lock == nil {
		lock = &cacheEntryLock{}
		cacheEntryLocks[directory] = lock
	}
	return lock
}

func retainCacheEntry(directory string) {
	lock := cacheEntryLockFor(directory)
	cacheEntryLocksMu.Lock()
	lock.refs++
	cacheEntryLocksMu.Unlock()
}

func releaseCacheEntry(directory string) {
	lock := cacheEntryLockFor(directory)
	cacheEntryLocksMu.Lock()
	if lock.refs > 0 {
		lock.refs--
	}
	cacheEntryLocksMu.Unlock()
}

func cacheEntryReferenced(directory string) bool {
	lock := cacheEntryLockFor(directory)
	cacheEntryLocksMu.Lock()
	defer cacheEntryLocksMu.Unlock()
	return lock.refs > 0
}
