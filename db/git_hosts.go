package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxGitHosts          = 32
	maxGitHostBytes      = 512
	maxGitUsernameBytes  = 255
	maxGitTokenBytes     = 16 << 10
	gitHostIDPrefix      = "git-"
	gitHostIDHexLength   = 16
	gitDefaultHostGitHub = "github.com"
)

// ErrGitHostNotFound reports an allowlist entry that does not resolve to a
// stored host.
var ErrGitHostNotFound = errors.New("git host not found")

// GitHost is one allowed Git host, safe to render in the settings page.
// TokenConfigured reports secret presence without exposing the token.
type GitHost struct {
	ID              string
	Host            string
	Username        string
	TokenConfigured bool
}

// GitHostConfig contains the username and token needed to clone from one host.
// Keep this type out of template and HTTP response data.
type GitHostConfig struct {
	Host     string
	Username string
	Token    string
}

// GitTokenChange selects how setGitHosts treats a stored host token. The zero
// value preserves the existing secret.
type GitTokenChange uint8

const (
	// GitKeepToken leaves the stored token untouched.
	GitKeepToken GitTokenChange = iota
	// GitReplaceToken stores TokenValue.
	GitReplaceToken
	// GitClearToken removes the stored token.
	GitClearToken
)

// GitHostUpdate changes one allowed host entry. TokenChange and TokenValue form
// an exhaustive tri-state decision about the stored secret.
type GitHostUpdate struct {
	Host        GitHost
	TokenChange GitTokenChange
	TokenValue  string
}

// gitHostStore is satisfied by both *sql.DB and *sql.Tx so host writes can run
// inside a shared transaction.
type gitHostStore interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// DefaultGitHosts returns the built-in public hosts used until an account
// saves its own allowlist. Entries have no tokens and stable synthetic IDs so
// the settings form can persist edits.
func DefaultGitHosts() []GitHost {
	hosts := []string{
		gitDefaultHostGitHub,
		"gitlab.com",
		"bitbucket.org",
		"codeberg.org",
	}
	values := make([]GitHost, 0, len(hosts))
	for index, host := range hosts {
		values = append(values, GitHost{
			ID:   fmt.Sprintf("%s%016x", gitHostIDPrefix, index+1),
			Host: host,
		})
	}
	return values
}

// ValidateGitHostID checks a host ID against the shared ID contract.
func ValidateGitHostID(id string) error {
	if len(id) != len(gitHostIDPrefix)+gitHostIDHexLength || !strings.HasPrefix(id, gitHostIDPrefix) {
		return fmt.Errorf("git host ID %q must hold %q plus %d lowercase hex characters", id, gitHostIDPrefix, gitHostIDHexLength)
	}
	for index := len(gitHostIDPrefix); index < len(id); index++ {
		if !isLowerHexDigit(id[index]) {
			return fmt.Errorf("git host ID %q has an invalid character at byte %d", id, index)
		}
	}
	return nil
}

// GetGitHosts returns the effective allowlist in stored order, safe for
// frontend rendering.
func GetGitHosts(ctx context.Context, db rowQueryer) ([]GitHost, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get git hosts: %w", err)
	}
	configured, err := gitHostsConfigured(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("get git hosts: %w", err)
	}
	if !configured {
		return DefaultGitHosts(), nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, host, COALESCE(username, ''),
			CASE WHEN token IS NOT NULL AND trim(token) <> '' THEN 1 ELSE 0 END
		FROM git_hosts
		WHERE user_id = ?
		ORDER BY position
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("get git hosts: %w", err)
	}
	defer rows.Close()
	values := []GitHost{}
	for rows.Next() {
		var value GitHost
		if err := rows.Scan(&value.ID, &value.Host, &value.Username, &value.TokenConfigured); err != nil {
			return nil, fmt.Errorf("scan git host: %w", err)
		}
		decryptedHost, err := decryptGitHostText(ctx, "git_hosts.host", value.ID, value.Host)
		if err != nil {
			return nil, fmt.Errorf("get git hosts: %w", err)
		}
		value.Host = decryptedHost
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate git hosts: %w", err)
	}
	return values, nil
}

// GetGitHostConfigs returns backend configurations for every effective host.
// Defaults carry empty tokens. Keep this result out of template and HTTP data.
func GetGitHostConfigs(ctx context.Context, db rowQueryer) ([]GitHostConfig, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get git host configs: %w", err)
	}
	configured, err := gitHostsConfigured(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("get git host configs: %w", err)
	}
	if !configured {
		defaults := DefaultGitHosts()
		configs := make([]GitHostConfig, 0, len(defaults))
		for _, host := range defaults {
			configs = append(configs, GitHostConfig{Host: host.Host})
		}
		return configs, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, host, COALESCE(username, ''), token
		FROM git_hosts
		WHERE user_id = ?
		ORDER BY position
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("get git host configs: %w", err)
	}
	defer rows.Close()
	configs := []GitHostConfig{}
	for rows.Next() {
		var id, host, username string
		var token sql.NullString
		if err := rows.Scan(&id, &host, &username, &token); err != nil {
			return nil, fmt.Errorf("scan git host config: %w", err)
		}
		decryptedHost, err := decryptGitHostText(ctx, "git_hosts.host", id, host)
		if err != nil {
			return nil, fmt.Errorf("get git host configs: %w", err)
		}
		config := GitHostConfig{Host: decryptedHost, Username: strings.TrimSpace(username)}
		if token.Valid {
			decryptedToken, err := decryptGitHostText(ctx, "git_hosts.token", id, token.String)
			if err != nil {
				return nil, fmt.Errorf("get git host configs: %w", err)
			}
			config.Token = decryptedToken
		}
		configs = append(configs, config)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate git host configs: %w", err)
	}
	return configs, nil
}

func gitHostsConfigured(ctx context.Context, db rowQueryer, userID int64) (bool, error) {
	var configured bool
	if err := db.QueryRowContext(ctx, `SELECT git_hosts_configured FROM settings WHERE id = ?`, userID).Scan(&configured); err != nil {
		return false, err
	}
	return configured, nil
}

// ValidateGitHosts checks IDs, hosts, token decisions, and collection size
// without writing.
func ValidateGitHosts(updates []GitHostUpdate) error {
	_, err := normalizeGitHosts(updates)
	return err
}

func normalizeGitHosts(updates []GitHostUpdate) ([]GitHostUpdate, error) {
	if len(updates) > maxGitHosts {
		return nil, fmt.Errorf("at most %d Git hosts are allowed", maxGitHosts)
	}
	normalized := make([]GitHostUpdate, 0, len(updates))
	seenIDs := make(map[string]struct{}, len(updates))
	seenHosts := make(map[string]struct{}, len(updates))
	for _, update := range updates {
		update.Host.ID = strings.TrimSpace(update.Host.ID)
		update.Host.Host = strings.TrimSpace(update.Host.Host)
		update.Host.Username = strings.TrimSpace(update.Host.Username)
		update.TokenValue = strings.TrimSpace(update.TokenValue)
		if err := ValidateGitHostID(update.Host.ID); err != nil {
			return nil, err
		}
		host, err := normalizeGitHost(update.Host.Host)
		if err != nil {
			return nil, fmt.Errorf("git host %q: %w", update.Host.ID, err)
		}
		update.Host.Host = host
		if err := validateGitUsername(update.Host.Username); err != nil {
			return nil, fmt.Errorf("git host %q: %w", update.Host.ID, err)
		}
		switch update.TokenChange {
		case GitKeepToken, GitClearToken:
			if update.TokenValue != "" {
				return nil, fmt.Errorf("git host %q token must be empty unless it is replaced", update.Host.ID)
			}
		case GitReplaceToken:
			if update.TokenValue == "" {
				return nil, fmt.Errorf("git host %q replacement token is required", update.Host.ID)
			}
			if err := validateGitToken(update.TokenValue); err != nil {
				return nil, fmt.Errorf("git host %q: %w", update.Host.ID, err)
			}
		default:
			return nil, fmt.Errorf("git host %q has unknown token change mode %d", update.Host.ID, update.TokenChange)
		}
		if _, ok := seenIDs[update.Host.ID]; ok {
			return nil, fmt.Errorf("git host %q is duplicated", update.Host.ID)
		}
		seenIDs[update.Host.ID] = struct{}{}
		hostKey := strings.ToLower(update.Host.Host)
		if _, ok := seenHosts[hostKey]; ok {
			return nil, fmt.Errorf("git host %q is duplicated", update.Host.Host)
		}
		seenHosts[hostKey] = struct{}{}
		normalized = append(normalized, update)
	}
	return normalized, nil
}

// normalizeGitHost converts a submitted host into a lowercase hostname plus
// optional non-default port. It rejects schemes, paths, credentials, queries,
// fragments, wildcards, and control characters.
func normalizeGitHost(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("host is required")
	}
	if len(value) > maxGitHostBytes {
		return "", fmt.Errorf("host exceeds %d bytes", maxGitHostBytes)
	}
	if strings.Contains(value, "://") {
		return "", errors.New("host must not include a scheme")
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) >= 0 {
		return "", errors.New("host must not contain whitespace or control characters")
	}
	if strings.ContainsAny(value, "/?#@\\") {
		return "", errors.New("host must be a hostname with an optional port")
	}
	if strings.Contains(value, "*") {
		return "", errors.New("host must not contain wildcards")
	}
	parsed, err := url.Parse("https://" + value)
	if err != nil || parsed.Host == "" {
		return "", errors.New("host is invalid")
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("host must be a hostname with an optional port")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return "", errors.New("host is required")
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("host port must be between 1 and 65535")
		}
		if number == 443 {
			port = ""
		} else {
			port = strconv.Itoa(number)
		}
	}
	if strings.Contains(host, ":") {
		if port != "" {
			return net.JoinHostPort(host, port), nil
		}
		return "[" + host + "]", nil
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

func validateGitUsername(value string) error {
	if len(value) > maxGitUsernameBytes {
		return fmt.Errorf("username exceeds %d bytes", maxGitUsernameBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("username must be valid UTF-8")
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) >= 0 {
		return errors.New("username must not contain whitespace or control characters")
	}
	if strings.Contains(value, ":") {
		return errors.New("username must not contain a colon")
	}
	return nil
}

func validateGitToken(value string) error {
	if len(value) > maxGitTokenBytes {
		return fmt.Errorf("token exceeds %d bytes", maxGitTokenBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("token must be valid UTF-8")
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character)
	}) >= 0 {
		return errors.New("token must not contain control characters")
	}
	return nil
}

// setGitHosts replaces the stored collection inside the caller's transaction.
// Tokens marked Keep are decrypted from the rows being replaced and
// re-encrypted with a fresh nonce.
func setGitHosts(ctx context.Context, db gitHostStore, updates []GitHostUpdate) error {
	userID, err := requireUserID(ctx)
	if err != nil {
		return fmt.Errorf("set git hosts: %w", err)
	}
	normalized, err := normalizeGitHosts(updates)
	if err != nil {
		return fmt.Errorf("set git hosts: %w", err)
	}

	existing, err := loadGitHostTokens(ctx, db)
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM git_hosts WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clear git hosts: %w", err)
	}
	for position, update := range normalized {
		encryptedHost, err := encryptGitHostText(ctx, "git_hosts.host", update.Host.ID, update.Host.Host)
		if err != nil {
			return fmt.Errorf("set git hosts: %w", err)
		}
		var token any
		switch update.TokenChange {
		case GitKeepToken:
			if value, ok := existing[update.Host.ID]; ok && strings.TrimSpace(value) != "" {
				encryptedToken, err := encryptGitHostText(ctx, "git_hosts.token", update.Host.ID, strings.TrimSpace(value))
				if err != nil {
					return fmt.Errorf("set git hosts: %w", err)
				}
				token = encryptedToken
			}
		case GitReplaceToken:
			encryptedToken, err := encryptGitHostText(ctx, "git_hosts.token", update.Host.ID, update.TokenValue)
			if err != nil {
				return fmt.Errorf("set git hosts: %w", err)
			}
			token = encryptedToken
		case GitClearToken:
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO git_hosts (user_id, id, position, host, username, token)
			VALUES (?, ?, ?, ?, ?, ?)
		`, userID, update.Host.ID, position, encryptedHost, update.Host.Username, token); err != nil {
			return fmt.Errorf("store git host %d: %w", position, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET git_hosts_configured = 1 WHERE id = ?`, userID); err != nil {
		return fmt.Errorf("mark git hosts configured: %w", err)
	}
	return nil
}

func loadGitHostTokens(ctx context.Context, db gitHostStore) (map[string]string, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("load git host tokens: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, token FROM git_hosts WHERE user_id = ? AND token IS NOT NULL`, userID)
	if err != nil {
		return nil, fmt.Errorf("load git host tokens: %w", err)
	}
	defer rows.Close()
	tokens := make(map[string]string)
	for rows.Next() {
		var id, token string
		if err := rows.Scan(&id, &token); err != nil {
			return nil, fmt.Errorf("scan git host token: %w", err)
		}
		decryptedToken, err := decryptGitHostText(ctx, "git_hosts.token", id, token)
		if err != nil {
			return nil, fmt.Errorf("load git host tokens: %w", err)
		}
		tokens[id] = decryptedToken
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate git host tokens: %w", err)
	}
	return tokens, nil
}

func encryptGitHostText(ctx context.Context, scope, rowID, value string) (string, error) {
	encrypted, err := EncryptStorageText(ctx, scope, rowID, value)
	if err != nil {
		return "", fmt.Errorf("encrypt %s: %w", scope, err)
	}
	return encrypted, nil
}

func decryptGitHostText(ctx context.Context, scope, rowID, value string) (string, error) {
	decrypted, err := DecryptStorageText(ctx, scope, rowID, value)
	if err != nil {
		return "", fmt.Errorf("decrypt %s: %w", scope, err)
	}
	return strings.TrimSpace(decrypted), nil
}
