// Package mcp connects remote MCP (Model Context Protocol) servers over the
// Streamable HTTP transport and exposes their tools as grouped capabilities
// compatible with the parent application's tool registry.
//
// Each connected server contributes exactly one selectable capability whose
// name is taken from the server configuration. Remote tools are converted to
// implementations of tools.CapabilityTool, so a connected server can be
// dropped into tools.NewRegistry like any built-in capability.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"seesharpsi/kritui/tools"
)

const (
	// connectTimeout bounds connection establishment plus tool discovery for
	// each server. It is intentionally short so a hanging remote cannot stall
	// application startup.
	connectTimeout = 15 * time.Second

	// maxToolNameLength mirrors the parent registry's tool name limit.
	maxToolNameLength = 64
)

// ServerConfig describes one remote MCP server to connect to over Streamable
// HTTP. AuthorizationToken is optional and is never exposed in errors or
// model-facing data.
type ServerConfig struct {
	Name               string
	URL                string
	AuthorizationToken string
	Capability         string
}

// Session is a set of connections to remote MCP servers. It is safe for
// concurrent use; individual tools execute concurrently against their server.
type Session struct {
	tools     []tools.Tool
	sessions  []*mcpsdk.ClientSession
	closeOnce sync.Once
	closeErr  error
}

// Connect establishes Streamable HTTP sessions with every configured server
// and discovers their tools. All configurations are validated before any
// network activity. If any server fails to connect, list, or convert its
// tools, every session opened so far is closed and an error is returned.
func Connect(ctx context.Context, configs []ServerConfig) (*Session, error) {
	for index, config := range configs {
		if err := validateConfig(config); err != nil {
			return nil, fmt.Errorf("mcp: server %d: %w", index, err)
		}
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "kritui", Version: "1"}, nil)
	session := &Session{}
	registeredNames := make(map[string]string)

	for _, config := range configs {
		remoteTools, sdkSession, err := connectServer(ctx, client, config)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		session.sessions = append(session.sessions, sdkSession)

		for _, remoteTool := range remoteTools {
			definitionName := remoteTool.Definition().Name
			if owner, exists := registeredNames[definitionName]; exists {
				_ = session.Close()
				return nil, fmt.Errorf("mcp: server %q: tool name %q duplicates the generated name of server %q", config.Name, definitionName, owner)
			}
			registeredNames[definitionName] = config.Name
			session.tools = append(session.tools, remoteTool)
		}
	}

	return session, nil
}

// Tools returns the discovered remote tools in deterministic connection
// order. The returned slice is a clone and may be modified freely.
func (s *Session) Tools() []tools.Tool {
	if s == nil {
		return nil
	}
	clone := make([]tools.Tool, len(s.tools))
	copy(clone, s.tools)
	return clone
}

// Close closes every underlying SDK session. It is idempotent and joins close
// errors from individual sessions.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var closeErrors []error
		for _, sdkSession := range s.sessions {
			if err := sdkSession.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		s.closeErr = errors.Join(closeErrors...)
	})
	return s.closeErr
}

func connectServer(ctx context.Context, client *mcpsdk.Client, config ServerConfig) ([]tools.Tool, *mcpsdk.ClientSession, error) {
	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   config.URL,
		HTTPClient: newHTTPClient(config.AuthorizationToken),
	}
	sdkSession, err := client.Connect(connCtx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: connect server %q: %w", config.Name, err)
	}

	remoteTools, err := listRemoteTools(connCtx, sdkSession, config)
	if err != nil {
		_ = sdkSession.Close()
		return nil, nil, err
	}
	return remoteTools, sdkSession, nil
}

func listRemoteTools(ctx context.Context, sdkSession *mcpsdk.ClientSession, config ServerConfig) ([]tools.Tool, error) {
	var remoteTools []tools.Tool
	for remote, err := range sdkSession.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: list tools: %w", config.Name, err)
		}
		remoteTool, err := newRemoteTool(sdkSession, config, remote)
		if err != nil {
			return nil, err
		}
		remoteTools = append(remoteTools, remoteTool)
	}
	return remoteTools, nil
}

var capabilityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateConfig(config ServerConfig) error {
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("mcp: server name is required")
	}
	if strings.TrimSpace(config.Capability) == "" {
		return errors.New("mcp: capability is required")
	}
	if len(config.Capability) > maxToolNameLength || !capabilityPattern.MatchString(config.Capability) {
		return fmt.Errorf("mcp: capability %q may contain only letters, digits, underscores, and hyphens, and must be at most %d characters", config.Capability, maxToolNameLength)
	}
	if strings.TrimSpace(config.URL) == "" {
		return errors.New("mcp: URL is required")
	}
	parsed, err := url.Parse(config.URL)
	if err != nil {
		return fmt.Errorf("mcp: URL is invalid: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("mcp: URL %q must be an absolute http or https URL with a host", config.URL)
	}
	if parsed.User != nil {
		return errors.New("mcp: URL must not contain userinfo")
	}
	if parsed.Fragment != "" {
		return errors.New("mcp: URL must not contain a fragment")
	}
	return nil
}
