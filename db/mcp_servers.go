package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxMCPServers             = 32
	maxMCPServerNameRunes     = 80
	maxMCPServerURLBytes      = 2048
	maxMCPServerTokenBytes    = 16 << 10
	mcpServerIDPrefix         = "mcp-"
	mcpServerIDHexLength      = 16
	mcpServerCapabilityPrefix = "mcp_server_"
)

// ErrMCPServerNotFound reports a selected MCP server capability that does not
// resolve to a stored server.
var ErrMCPServerNotFound = errors.New("mcp server not found")

// MCPServer is one configured MCP server entry, safe to render in the settings
// page. AuthorizationConfigured reports secret presence without exposing the
// secret; the token itself never appears on this type.
type MCPServer struct {
	ID                      string
	Name                    string
	URL                     string
	AuthorizationConfigured bool
}

// MCPServerConfig contains the authorization token needed for backend tool
// calls. Keep this type out of template and HTTP response data.
type MCPServerConfig struct {
	ID                 string
	Name               string
	URL                string
	AuthorizationToken string
}

// MCPAuthorizationChange selects how setMCPServers treats the stored
// authorization token. The zero value preserves the existing secret.
type MCPAuthorizationChange uint8

const (
	// MCPKeepAuthorization leaves the stored token untouched, and stores NULL
	// for newly added servers.
	MCPKeepAuthorization MCPAuthorizationChange = iota
	// MCPReplaceAuthorization stores AuthorizationValue.
	MCPReplaceAuthorization
	// MCPClearAuthorization removes the stored token.
	MCPClearAuthorization
)

// MCPServerUpdate changes one MCP server entry. AuthorizationChange and
// AuthorizationValue form an exhaustive tri-state decision about the stored
// secret, so contradictory instructions are unrepresentable.
type MCPServerUpdate struct {
	Server              MCPServer
	AuthorizationChange MCPAuthorizationChange
	AuthorizationValue  string
}

// mcpServerStore is satisfied by both *sql.DB and *sql.Tx so MCP server writes
// can run inside a shared transaction.
type mcpServerStore interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// GetMCPServers returns configured servers in stored order, safe for frontend
// rendering.
func GetMCPServers(ctx context.Context, db rowQueryer) ([]MCPServer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, url,
			CASE WHEN authorization_token IS NOT NULL AND trim(authorization_token) <> '' THEN 1 ELSE 0 END
		FROM mcp_servers
		ORDER BY position
	`)
	if err != nil {
		return nil, fmt.Errorf("get mcp servers: %w", err)
	}
	defer rows.Close()
	values := []MCPServer{}
	for rows.Next() {
		var value MCPServer
		if err := rows.Scan(&value.ID, &value.Name, &value.URL, &value.AuthorizationConfigured); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}
		value.Name = strings.TrimSpace(value.Name)
		value.URL = strings.TrimSpace(value.URL)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mcp servers: %w", err)
	}
	return values, nil
}

// GetMCPServerConfigs resolves selected stable capabilities to backend
// configurations in submitted capability order. Unknown or repeated
// capabilities are rejected, and nonselected servers are never returned.
func GetMCPServerConfigs(ctx context.Context, db rowQueryer, capabilities []string) ([]MCPServerConfig, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, url, authorization_token
		FROM mcp_servers
	`)
	if err != nil {
		return nil, fmt.Errorf("get mcp server configs: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]MCPServerConfig)
	for rows.Next() {
		var config MCPServerConfig
		var token sql.NullString
		if err := rows.Scan(&config.ID, &config.Name, &config.URL, &token); err != nil {
			return nil, fmt.Errorf("scan mcp server config: %w", err)
		}
		config.Name = strings.TrimSpace(config.Name)
		config.URL = strings.TrimSpace(config.URL)
		if token.Valid {
			config.AuthorizationToken = strings.TrimSpace(token.String)
		}
		byID[config.ID] = config
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mcp server configs: %w", err)
	}

	selected := make([]MCPServerConfig, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		id, ok := ParseMCPServerCapability(strings.TrimSpace(capability))
		if !ok {
			return nil, fmt.Errorf("get mcp server config: %w: unknown capability %q", ErrMCPServerNotFound, capability)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("get mcp server config: mcp server %q selected more than once", id)
		}
		config, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("get mcp server config: %w: %q", ErrMCPServerNotFound, id)
		}
		seen[id] = struct{}{}
		selected = append(selected, config)
	}
	return selected, nil
}

// MCPServerCapability returns the stable tools.Registry function name for one
// server: the "mcp_server_" prefix plus the generated ID payload. Valid IDs
// produce names well under the 64-byte tool name limit.
func MCPServerCapability(id string) string {
	return mcpServerCapabilityPrefix + strings.TrimPrefix(id, mcpServerIDPrefix)
}

// ParseMCPServerCapability reverses MCPServerCapability for exact valid
// capability names and reports whether the name matched.
func ParseMCPServerCapability(name string) (string, bool) {
	payload, ok := strings.CutPrefix(name, mcpServerCapabilityPrefix)
	if !ok {
		return "", false
	}
	id := mcpServerIDPrefix + payload
	if ValidateMCPServerID(id) != nil {
		return "", false
	}
	return id, true
}

// ValidateMCPServerID checks a server ID against the single ID contract shared
// by every path that persists or references servers. A valid ID holds "mcp-"
// plus exactly 16 lowercase hex characters, so capability names derived from
// it stay valid tools.Registry function names.
func ValidateMCPServerID(id string) error {
	if len(id) != len(mcpServerIDPrefix)+mcpServerIDHexLength || !strings.HasPrefix(id, mcpServerIDPrefix) {
		return fmt.Errorf("mcp server ID %q must hold %q plus %d lowercase hex characters", id, mcpServerIDPrefix, mcpServerIDHexLength)
	}
	for index := len(mcpServerIDPrefix); index < len(id); index++ {
		if !isLowerHexDigit(id[index]) {
			return fmt.Errorf("mcp server ID %q has an invalid character at byte %d", id, index)
		}
	}
	return nil
}

func isLowerHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f'
}

// ValidateMCPServers checks IDs, names, URLs, token decisions, and collection
// size before any persistence path stores the collection.
func ValidateMCPServers(updates []MCPServerUpdate) error {
	_, err := normalizeMCPServers(updates)
	return err
}

// normalizeMCPServers trims every submitted value and enforces the collection
// contract, so persistence always stores trimmed values.
func normalizeMCPServers(updates []MCPServerUpdate) ([]MCPServerUpdate, error) {
	if len(updates) > maxMCPServers {
		return nil, fmt.Errorf("at most %d MCP servers are allowed", maxMCPServers)
	}
	normalized := make([]MCPServerUpdate, 0, len(updates))
	seenIDs := make(map[string]struct{}, len(updates))
	seenNames := make(map[string]struct{}, len(updates))
	for _, update := range updates {
		update.Server.ID = strings.TrimSpace(update.Server.ID)
		update.Server.Name = strings.TrimSpace(update.Server.Name)
		update.Server.URL = strings.TrimSpace(update.Server.URL)
		update.AuthorizationValue = strings.TrimSpace(update.AuthorizationValue)
		if err := ValidateMCPServerID(update.Server.ID); err != nil {
			return nil, err
		}
		if update.Server.Name == "" {
			return nil, fmt.Errorf("mcp server %q name is required", update.Server.ID)
		}
		if utf8.RuneCountInString(update.Server.Name) > maxMCPServerNameRunes {
			return nil, fmt.Errorf("mcp server %q name exceeds %d characters", update.Server.ID, maxMCPServerNameRunes)
		}
		if err := validateMCPServerURL(update.Server.ID, update.Server.URL); err != nil {
			return nil, err
		}
		switch update.AuthorizationChange {
		case MCPKeepAuthorization, MCPClearAuthorization:
			if update.AuthorizationValue != "" {
				return nil, fmt.Errorf("mcp server %q authorization value must be empty unless the token is replaced", update.Server.ID)
			}
		case MCPReplaceAuthorization:
			if update.AuthorizationValue == "" {
				return nil, fmt.Errorf("mcp server %q replacement authorization token is required", update.Server.ID)
			}
			if len(update.AuthorizationValue) > maxMCPServerTokenBytes {
				return nil, fmt.Errorf("mcp server %q authorization token exceeds %d bytes", update.Server.ID, maxMCPServerTokenBytes)
			}
		default:
			return nil, fmt.Errorf("mcp server %q has unknown authorization change mode %d", update.Server.ID, update.AuthorizationChange)
		}
		if _, ok := seenIDs[update.Server.ID]; ok {
			return nil, fmt.Errorf("mcp server %q is duplicated", update.Server.ID)
		}
		seenIDs[update.Server.ID] = struct{}{}
		nameKey := strings.ToLower(update.Server.Name)
		if _, ok := seenNames[nameKey]; ok {
			return nil, fmt.Errorf("mcp server name %q is duplicated", update.Server.Name)
		}
		seenNames[nameKey] = struct{}{}
		normalized = append(normalized, update)
	}
	return normalized, nil
}

func validateMCPServerURL(id, value string) error {
	if value == "" {
		return fmt.Errorf("mcp server %q URL is required", id)
	}
	if len(value) > maxMCPServerURLBytes {
		return fmt.Errorf("mcp server %q URL exceeds %d bytes", id, maxMCPServerURLBytes)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("mcp server %q URL %q is invalid: %v", id, value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("mcp server %q URL %q must use http or https", id, value)
	}
	if parsed.User != nil {
		return fmt.Errorf("mcp server %q URL %q must not embed credentials", id, value)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("mcp server %q URL %q must not contain a fragment", id, value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("mcp server %q URL %q must include a host", id, value)
	}
	return nil
}

// setMCPServers replaces the stored collection inside the caller's
// transaction. It never begins, commits, or rolls back. Tokens marked Keep are
// copied from the rows being replaced, Replace stores the submitted value, and
// Clear stores NULL. Stable capability names of removed servers are pruned
// from default_tools and chat_tools; built-in names are never touched.
func setMCPServers(ctx context.Context, db mcpServerStore, updates []MCPServerUpdate) error {
	normalized, err := normalizeMCPServers(updates)
	if err != nil {
		return fmt.Errorf("set mcp servers: %w", err)
	}

	existing, err := loadMCPServerTokens(ctx, db)
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM mcp_servers`); err != nil {
		return fmt.Errorf("clear mcp servers: %w", err)
	}
	for position, update := range normalized {
		var token any
		switch update.AuthorizationChange {
		case MCPKeepAuthorization:
			if value, ok := existing[update.Server.ID]; ok && strings.TrimSpace(value) != "" {
				token = value
			}
		case MCPReplaceAuthorization:
			token = update.AuthorizationValue
		case MCPClearAuthorization:
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO mcp_servers (id, position, name, url, authorization_token)
			VALUES (?, ?, ?, ?, ?)
		`, update.Server.ID, position, update.Server.Name, update.Server.URL, token); err != nil {
			return fmt.Errorf("store mcp server %q: %w", update.Server.ID, err)
		}
	}

	kept := make(map[string]struct{}, len(normalized))
	for _, update := range normalized {
		kept[update.Server.ID] = struct{}{}
	}
	for id := range existing {
		if _, ok := kept[id]; ok {
			continue
		}
		capability := MCPServerCapability(id)
		if _, err := db.ExecContext(ctx, `DELETE FROM default_tools WHERE name = ?`, capability); err != nil {
			return fmt.Errorf("prune default tool %q: %w", capability, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM chat_tools WHERE name = ?`, capability); err != nil {
			return fmt.Errorf("prune chat tool %q: %w", capability, err)
		}
	}
	return nil
}

// loadMCPServerTokens maps every stored server ID to its raw token value,
// with blank and NULL tokens represented by an empty string so removed
// servers can be detected regardless of their stored secret.
func loadMCPServerTokens(ctx context.Context, db mcpServerStore) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, authorization_token FROM mcp_servers`)
	if err != nil {
		return nil, fmt.Errorf("load existing mcp servers: %w", err)
	}
	defer rows.Close()
	existing := make(map[string]string)
	for rows.Next() {
		var id string
		var token sql.NullString
		if err := rows.Scan(&id, &token); err != nil {
			return nil, fmt.Errorf("scan existing mcp server: %w", err)
		}
		if token.Valid {
			existing[id] = token.String
		} else {
			existing[id] = ""
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing mcp servers: %w", err)
	}
	return existing, nil
}
