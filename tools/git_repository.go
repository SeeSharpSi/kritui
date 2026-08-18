package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	gitMaxRepositories                 = 8
	gitMaxRepositoriesPerSession       = 4
	gitCloneTimeout                    = 90 * time.Second
	gitOperationTimeout                = 20 * time.Second
	gitResourceCheckInterval           = time.Second
	gitRepositoryMaxSize         int64 = 256 * 1024 * 1024
	gitFreeSpaceReserve          int64 = 512 * 1024 * 1024
	gitMaxResultBytes                  = 16 * 1024
	gitMaxErrorBytes                   = 4 * 1024
	gitMaxInputLineBytes               = 1024 * 1024
	gitMaxPathLength                   = 4096
	gitMaxQueryLength                  = 512
	gitMaxRefLength                    = 256
	gitMaxOffset                       = 100000
)

var gitAllowedHosts = map[string]struct{}{
	"bitbucket.org": {},
	"github.com":    {},
	"gitlab.com":    {},
}

type gitSessionContextKey struct{}

// GitSession owns temporary repository snapshots used during one completion.
// A session is safe for concurrent tool calls. Close removes every clone.
type GitSession struct {
	mu           sync.Mutex
	closed       bool
	repositories map[*gitRuntime]map[string]*gitRepository
}

type gitRuntime struct {
	mu        sync.Mutex
	active    int
	orphans   map[string]struct{}
	removeAll func(string) error
}

type gitRepository struct {
	mu        sync.Mutex
	runtime   *gitRuntime
	root      string
	directory string
	commit    string
}

func newGitRuntime() *gitRuntime {
	return &gitRuntime{
		orphans:   make(map[string]struct{}),
		removeAll: os.RemoveAll,
	}
}

// NewGitSession creates an empty request-scoped Git session.
func NewGitSession() *GitSession {
	return &GitSession{repositories: make(map[*gitRuntime]map[string]*gitRepository)}
}

// Context returns a child context through which Git tools access this session.
func (s *GitSession) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, gitSessionContextKey{}, s)
}

// Close removes all repositories held by the session. Cleanup failures remain
// reserved and are retried before a later clone.
func (s *GitSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var repositories []*gitRepository
	for _, byKey := range s.repositories {
		for _, repository := range byKey {
			repositories = append(repositories, repository)
		}
	}
	s.repositories = nil
	s.mu.Unlock()

	var cleanupErrors []error
	for _, repository := range repositories {
		if err := repository.runtime.removeRepository(repository); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func (r *gitRuntime) withRepository(ctx context.Context, address, ref string, operation func(*gitRepository) (string, error)) (string, error) {
	session, ok := ctx.Value(gitSessionContextKey{}).(*GitSession)
	if !ok || session == nil {
		return "", errors.New("git: request session is unavailable")
	}
	repository, err := r.repository(ctx, session, address, ref)
	if err != nil {
		return "", err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	result, err := operation(repository)
	if err == nil && len(result) > gitMaxResultBytes {
		return "", errors.New("git: result exceeded output limit")
	}
	return result, err
}

func (r *gitRuntime) repository(ctx context.Context, session *GitSession, address, ref string) (*gitRepository, error) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		if err := validateGitRef(ref); err != nil {
			return nil, err
		}
	}
	key := address + "\x00" + ref

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil, errors.New("git: request session is closed")
	}
	byKey := session.repositories[r]
	if byKey == nil {
		byKey = make(map[string]*gitRepository)
		session.repositories[r] = byKey
	}
	if repository := byKey[key]; repository != nil {
		return repository, nil
	}
	if len(byKey) >= gitMaxRepositoriesPerSession {
		return nil, fmt.Errorf("git: at most %d repositories may be inspected in one response", gitMaxRepositoriesPerSession)
	}

	repository, err := r.clone(ctx, address, ref)
	if err != nil {
		return nil, err
	}
	byKey[key] = repository
	return repository, nil
}

func (r *gitRuntime) clone(ctx context.Context, address, ref string) (_ *gitRepository, resultErr error) {
	if err := r.reserveRepository(os.TempDir()); err != nil {
		return nil, err
	}
	reserved := true
	root := ""
	defer func() {
		if !reserved {
			return
		}
		if root == "" {
			r.releaseRepository()
			return
		}
		if err := r.removeAll(root); err != nil {
			r.rememberOrphan(root)
			resultErr = errors.Join(resultErr, fmt.Errorf("git: clean failed clone: %w", err))
			return
		}
		r.releaseRepository()
	}()

	var err error
	root, err = os.MkdirTemp("", "kritui-git-")
	if err != nil {
		return nil, fmt.Errorf("git: create temporary directory: %w", err)
	}
	directory := filepath.Join(root, "repository.git")
	cloneArgs := []string{"clone", "--bare", "--filter=blob:none", "--single-branch", "--no-tags"}
	if ref != "" {
		cloneArgs = append(cloneArgs, "--branch", ref)
	}
	cloneArgs = append(cloneArgs, "--", address, directory)
	if err := runGitCommand(ctx, gitCommandOptions{
		resourceRoot: root,
		timeout:      gitCloneTimeout,
	}, discardGitOutput, cloneArgs...); err != nil {
		return nil, fmt.Errorf("git: clone repository: %w", err)
	}

	commit, err := resolveGitCommit(ctx, &gitRepository{root: root, directory: directory}, "")
	if err != nil {
		return nil, fmt.Errorf("git: resolve selected ref: %w", err)
	}
	repository := &gitRepository{
		runtime:   r,
		root:      root,
		directory: directory,
		commit:    commit,
	}
	reserved = false
	return repository, nil
}

func (r *gitRuntime) removeRepository(repository *gitRepository) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if err := r.removeAll(repository.root); err != nil {
		r.rememberOrphan(repository.root)
		return fmt.Errorf("git: remove temporary repository: %w", err)
	}
	r.releaseRepository()
	return nil
}

func (r *gitRuntime) reserveRepository(directory string) error {
	r.retryOrphans()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active >= gitMaxRepositories {
		return fmt.Errorf("git: at most %d repository snapshots may be active", gitMaxRepositories)
	}
	available, err := availableDiskBytes(directory)
	if err != nil {
		return fmt.Errorf("git: check free disk space: %w", err)
	}
	required := uint64(gitFreeSpaceReserve) + uint64(r.active+1)*uint64(gitRepositoryMaxSize)
	if available < required {
		return fmt.Errorf("git: insufficient free disk space: need at least %s, have %s", formatGitBytes(required), formatGitBytes(available))
	}
	r.active++
	return nil
}

func (r *gitRuntime) releaseRepository() {
	r.mu.Lock()
	if r.active > 0 {
		r.active--
	}
	r.mu.Unlock()
}

func (r *gitRuntime) rememberOrphan(root string) {
	r.mu.Lock()
	r.orphans[root] = struct{}{}
	r.mu.Unlock()
}

func (r *gitRuntime) retryOrphans() {
	r.mu.Lock()
	roots := make([]string, 0, len(r.orphans))
	for root := range r.orphans {
		roots = append(roots, root)
	}
	r.mu.Unlock()
	for _, root := range roots {
		if err := r.removeAll(root); err != nil {
			continue
		}
		r.mu.Lock()
		if _, exists := r.orphans[root]; exists {
			delete(r.orphans, root)
			if r.active > 0 {
				r.active--
			}
		}
		r.mu.Unlock()
	}
}

func requiredGitPath(value string) (string, error) {
	if value == "" {
		return "", errors.New("git: path is required")
	}
	cleaned, err := validateGitPath(value)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return "", errors.New("git: path must identify a file")
	}
	return cleaned, nil
}

func optionalGitPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return validateGitPath(value)
}

func validateGitURL(address string) (string, error) {
	address = strings.TrimSpace(address)
	if len(address) > 2048 {
		return "", errors.New("git: repository URL exceeds 2048 bytes")
	}
	if hasUnsafeGitControl(address) {
		return "", errors.New("git: repository URL must not contain control characters")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if parsed.User != nil {
		return "", errors.New("git: repository URL must not contain credentials")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("git: repository URL must use the default HTTPS port")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("git: repository URL must not contain a query or fragment")
	}
	if _, allowed := gitAllowedHosts[strings.ToLower(parsed.Hostname())]; !allowed {
		return "", errors.New("git: repository host must be github.com, gitlab.com, or bitbucket.org")
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return "", errors.New("git: repository URL must contain a repository path")
	}
	return parsed.String(), nil
}

func validateGitRef(ref string) error {
	if ref == "" {
		return errors.New("git: ref must not be empty")
	}
	if len(ref) > gitMaxRefLength {
		return fmt.Errorf("git: ref exceeds %d bytes", gitMaxRefLength)
	}
	if strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, ".") || strings.HasSuffix(ref, ".") ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return errors.New("git: ref is not a valid branch or tag")
	}
	for _, character := range ref {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("-._/", character) {
			continue
		}
		return errors.New("git: ref is not a valid branch or tag")
	}
	return nil
}

func validateGitPath(repositoryPath string) (string, error) {
	if len(repositoryPath) > gitMaxPathLength {
		return "", fmt.Errorf("git: path exceeds %d bytes", gitMaxPathLength)
	}
	if hasUnsafeGitControl(repositoryPath) {
		return "", errors.New("git: path must not contain control characters")
	}
	if strings.HasPrefix(repositoryPath, "/") {
		return "", errors.New("git: path must be repository-relative")
	}
	for component := range strings.SplitSeq(repositoryPath, "/") {
		if component == ".." {
			return "", errors.New("git: path must not contain parent traversal")
		}
	}
	cleaned := path.Clean(repositoryPath)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("git: path must not traverse outside the repository")
	}
	return cleaned, nil
}

func gitPage(offsetValue, limitValue *int, defaultLimit, maxLimit int) (int, int, error) {
	offset := 0
	if offsetValue != nil {
		offset = *offsetValue
	}
	if offset < 0 || offset > gitMaxOffset {
		return 0, 0, fmt.Errorf("git: offset must be between 0 and %d", gitMaxOffset)
	}
	limit := defaultLimit
	if limitValue != nil {
		limit = *limitValue
	}
	if limit < 1 || limit > maxLimit {
		return 0, 0, fmt.Errorf("git: limit must be between 1 and %d", maxLimit)
	}
	return offset, limit, nil
}

func isHexCommit(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func literalGitPathspec(repositoryPath string) string {
	return ":(literal)" + repositoryPath
}
