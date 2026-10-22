package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LegacyOwnerID is the reserved account that owns every chat and setting
// created before multi-user persistence. Authentication middleware always
// installs an explicit user context; only trusted direct database callers and
// tests may rely on the legacy default.
const LegacyOwnerID int64 = 1

// LegacyOwnerUsername is the canonical name of the reserved legacy account.
// Signing up with this case-insensitive username claims the existing row
// instead of creating another account.
const LegacyOwnerUsername = "Skrittle"

const (
	usernameMinLength = 3
	usernameMaxLength = 32
)

var (
	// ErrUsernameTaken reports a case-insensitive username collision.
	ErrUsernameTaken = errors.New("username already taken")
	// ErrInvalidUser reports a nonpositive user ID in an explicit user context.
	// It never falls back to the legacy owner.
	ErrInvalidUser = errors.New("invalid user")
)

// User is one account. PasswordHash is the encoded password verifier; callers
// must hash and verify passwords themselves.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

type userContextKey struct{}

// WithUser returns a context scoped to userID. Auth middleware must call this
// after resolving a session. An explicit nonpositive ID is retained and later
// rejected by database operations; it never silently becomes the legacy owner.
func WithUser(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userContextKey{}, userID)
}

// UserID returns the explicit user ID in ctx. An absent value means the trusted
// legacy owner ID 1, preserving backward compatibility for direct database
// callers and tests. HTTP handlers must always supply WithUser.
func UserID(ctx context.Context) int64 {
	if ctx == nil {
		return LegacyOwnerID
	}
	if userID, ok := ctx.Value(userContextKey{}).(int64); ok {
		return userID
	}
	return LegacyOwnerID
}

func requireUserID(ctx context.Context) (int64, error) {
	userID := UserID(ctx)
	if userID <= 0 {
		return 0, ErrInvalidUser
	}
	return userID, nil
}

// ValidateUsername checks the shared username policy: 3-32 ASCII characters,
// starting with a letter, digit, or underscore, followed by letters, digits,
// underscores, dots, or hyphens. Usernames are case-insensitive for uniqueness
// while their original spelling is preserved.
func ValidateUsername(username string) error {
	if len(username) < usernameMinLength || len(username) > usernameMaxLength {
		return fmt.Errorf("username must be between %d and %d characters", usernameMinLength, usernameMaxLength)
	}
	for index := 0; index < len(username); index++ {
		b := username[index]
		if index == 0 {
			if !isUsernameInitial(b) {
				return fmt.Errorf("username must start with a letter, digit, or underscore")
			}
			continue
		}
		if !isUsernameCharacter(b) {
			return fmt.Errorf("username contains an invalid character at byte %d", index)
		}
	}
	return nil
}

func isUsernameInitial(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

func isUsernameCharacter(b byte) bool {
	return isUsernameInitial(b) || b == '.' || b == '-'
}

// CreateUser inserts an account and its default settings row atomically. The
// caller supplies an already encoded password hash. New accounts inherit
// built-in defaults and never copy another account's secrets or customization.
//
// A signup with the case-insensitive legacy username claims the unclaimed
// legacy account in row 1 instead of inserting a new row. The conditional
// update runs inside the write transaction, so concurrent claims cannot
// overwrite a password that another signup just stored.
func CreateUser(ctx context.Context, database *sql.DB, username, passwordHash string) (User, error) {
	username = strings.TrimSpace(username)
	passwordHash = strings.TrimSpace(passwordHash)
	if err := ValidateUsername(username); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if passwordHash == "" {
		return User{}, fmt.Errorf("create user: password hash is required")
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer tx.Rollback()

	if strings.EqualFold(username, LegacyOwnerUsername) {
		var claimed User
		err := tx.QueryRowContext(ctx, `
			UPDATE users
			SET password_hash = ?, disabled = 0
			WHERE id = ?
				AND username = ? COLLATE BINARY
				AND password_hash = ''
			RETURNING id, username, password_hash
		`, passwordHash, LegacyOwnerID, LegacyOwnerUsername).Scan(
			&claimed.ID, &claimed.Username, &claimed.PasswordHash,
		)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO settings (id) VALUES (?)
			`, LegacyOwnerID); err != nil {
				return User{}, fmt.Errorf("ensure legacy account settings: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return User{}, fmt.Errorf("commit create user: %w", err)
			}
			return claimed, nil
		case !errors.Is(err, sql.ErrNoRows):
			return User{}, fmt.Errorf("claim legacy account: %w", err)
		}
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, disabled)
		VALUES (?, ?, 0)
	`, username, passwordHash)
	if err != nil {
		if isUsernameConstraint(err) {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("get created user ID: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (id) VALUES (?)`, userID); err != nil {
		return User{}, fmt.Errorf("create user settings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create user: %w", err)
	}
	return User{ID: userID, Username: username, PasswordHash: passwordHash}, nil
}

// GetUserByUsername resolves a case-insensitive username for an enabled
// account. Unknown and disabled accounts return sql.ErrNoRows.
func GetUserByUsername(ctx context.Context, database *sql.DB, username string) (User, error) {
	username = strings.TrimSpace(username)
	var user User
	err := database.QueryRowContext(ctx, `
		SELECT id, username, password_hash
		FROM users
		WHERE username = ? AND disabled = 0
	`, username).Scan(&user.ID, &user.Username, &user.PasswordHash)
	if err != nil {
		return User{}, fmt.Errorf("get user by username: %w", err)
	}
	return user, nil
}

// CreateSession stores a session token hash and expiry. Expired sessions are
// removed first. The raw token is never stored.
func CreateSession(ctx context.Context, database *sql.DB, userID int64, tokenHash string, expiresAt time.Time) error {
	if userID <= 0 {
		return fmt.Errorf("create session: %w", ErrInvalidUser)
	}
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return fmt.Errorf("create session: token hash is required")
	}
	if expiresAt.IsZero() {
		return fmt.Errorf("create session: expiry is required")
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create session: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE expires_at <= ?
	`, time.Now().UTC().UnixMilli()); err != nil {
		return fmt.Errorf("clean expired sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES (?, ?, ?)
	`, tokenHash, userID, expiresAt.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create session: %w", err)
	}
	return nil
}

// GetSessionUser resolves an unexpired session token hash to an enabled user.
// Unknown, expired, and disabled sessions return sql.ErrNoRows.
func GetSessionUser(ctx context.Context, database *sql.DB, tokenHash string, now time.Time) (User, error) {
	tokenHash = strings.TrimSpace(tokenHash)
	var user User
	err := database.QueryRowContext(ctx, `
		SELECT users.id, users.username, users.password_hash
		FROM sessions
		JOIN users ON users.id = sessions.user_id
		WHERE sessions.token_hash = ?
			AND sessions.expires_at > ?
			AND users.disabled = 0
	`, tokenHash, now.UTC().UnixMilli()).Scan(&user.ID, &user.Username, &user.PasswordHash)
	if err != nil {
		return User{}, fmt.Errorf("get session user: %w", err)
	}
	return user, nil
}

// DeleteSession revokes one session token hash. Revoking an unknown session is
// a no-op.
func DeleteSession(ctx context.Context, database *sql.DB, tokenHash string) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return nil
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE token_hash = ?
	`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func isUsernameConstraint(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") &&
		(strings.Contains(message, "users.username") || strings.Contains(message, "users_username_unique"))
}
