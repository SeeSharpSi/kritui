package git

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// HostConfig associates one canonical HTTPS host with an optional username
// and token. Credentials are only used for that host and are never exposed to
// the model.
type HostConfig struct {
	Host     string
	Username string
	Token    string
}

// SessionConfig describes one request-scoped Git session.
type SessionConfig struct {
	UserID int64
	Hosts  []HostConfig
}

type gitSessionContextKey struct{}

type gitHostCredential struct {
	username string
	token    string
}

// GitSession owns per-session cache references. Closing a session releases
// references but never deletes persistent cache entries.
type GitSession struct {
	mu           sync.Mutex
	closed       bool
	userID       int64
	hosts        map[string]gitHostCredential
	repositories map[string]*gitRepository
}

// NewGitSession creates a request-scoped Git session. Invalid host entries are
// ignored; a URL can only use an exact normalized host from this allowlist.
func NewGitSession(config SessionConfig) *GitSession {
	hosts := make(map[string]gitHostCredential, len(config.Hosts))
	for _, host := range config.Hosts {
		canonical, err := parseHostConfig(host.Host)
		if err != nil {
			continue
		}
		username := strings.TrimSpace(host.Username)
		token := host.Token
		if hasUnsafeGitControl(username) {
			username = ""
		}
		if hasUnsafeGitControl(token) {
			token = ""
		}
		hosts[canonical] = gitHostCredential{username: username, token: token}
	}
	return &GitSession{
		userID:       config.UserID,
		hosts:        hosts,
		repositories: make(map[string]*gitRepository),
	}
}

// Context returns a child context through which Git tools access this session.
func (s *GitSession) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, gitSessionContextKey{}, s)
}

// Close releases every cache reference held by the session. Cache directories
// remain on disk for reuse by later sessions.
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
	repositories := make([]*gitRepository, 0, len(s.repositories))
	for _, repository := range s.repositories {
		repositories = append(repositories, repository)
	}
	s.repositories = nil
	s.hosts = nil
	s.mu.Unlock()

	for _, repository := range repositories {
		releaseCacheEntry(repository.directory)
	}
	return nil
}

func (s *GitSession) repositoryTarget(rawURL string) (string, string, string, error) {
	parsed, err := parseRepositoryURL(rawURL)
	if err != nil {
		return "", "", "", err
	}
	credential, allowed := s.hosts[parsed.host]
	if !allowed {
		return "", "", "", &gitHostNotAllowedError{host: parsed.host}
	}
	return parsed.address, credential.username, credential.token, nil
}

type gitHostNotAllowedError struct {
	host string
}

func (err *gitHostNotAllowedError) Error() string {
	return fmt.Sprintf("git: repository host %q is not allowed", err.host)
}
