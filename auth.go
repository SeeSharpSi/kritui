package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	kritui_db "seesharpsi/kritui/db"
)

const (
	authSessionCookieName = "kritui_session"
	authSessionTokenBytes = 32
	authSessionLifetime   = 30 * 24 * time.Hour
	authSessionMaxAge     = 30 * 24 * 60 * 60

	authFormMaxBytes = 16 << 10

	authRateLimitAttempts   = 10
	authRateLimitWindow     = time.Minute
	authRateLimitMaxEntries = 4096

	authNextMaxBytes = 2048
)

// authDummyPasswordHash is computed lazily so a login attempt for an unknown
// username still performs the same bounded password-derivation work as a real
// account. The value is not a credential and is never used for authentication.
var authDummyPasswordHash = sync.OnceValue(func() string {
	encoded, err := hashPassword("kritui-dummy-password")
	if err != nil {
		panic("create dummy password hash: " + err.Error())
	}
	return encoded
})

// authService owns browser session authentication for the application. It is
// intentionally separate from the application handlers: registerRoutes adds
// the account routes, and middleware wraps the complete mux so every
// non-public route receives a database-backed user identity.
type authService struct {
	database      *sql.DB
	secureCookies bool
	now           func() time.Time

	loginLimiter  *authRateLimiter
	signupLimiter *authRateLimiter
}

// newAuthService constructs the authentication service. secureCookies should
// come from KRITUI_SECURE_COOKIES; direct TLS requests always receive Secure
// cookies even when this value is false.
func newAuthService(database *sql.DB, secureCookies bool) *authService {
	return &authService{
		database:      database,
		secureCookies: secureCookies,
		now:           time.Now,
		loginLimiter:  newAuthRateLimiter(authRateLimitAttempts, authRateLimitWindow),
		signupLimiter: newAuthRateLimiter(authRateLimitAttempts, authRateLimitWindow),
	}
}

// registerRoutes adds the account endpoints to mux. The caller is responsible
// for wrapping the mux with middleware and for applying net/http's
// CrossOriginProtection around the complete application.
func (a *authService) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.handleLoginPage)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("GET /signup", a.handleSignupPage)
	mux.HandleFunc("POST /signup", a.handleSignup)
	mux.HandleFunc("POST /logout", a.handleLogout)
}

// middleware requires a live session for every route except public assets,
// health checks, and the account pages themselves. A valid session installs
// the actual stored user ID in the request context. Unauthenticated navigation
// is redirected to the sign-in page; htmx requests receive HX-Redirect instead
// of an HTML login page that could be swapped into an application panel.
func (a *authService) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPublicRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Cookie")

		user, active, err := a.sessionUser(r)
		if err != nil {
			log.Printf("auth: look up session: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if !active {
			a.redirectToLogin(w, r)
			return
		}

		ctx := kritui_db.WithUser(r.Context(), user.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func authPublicRequest(r *http.Request) bool {
	path := r.URL.Path
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		switch path {
		case "/sw.js", "/logo.png", "/favicon.ico", "/healthz":
			return true
		}
		if strings.HasPrefix(path, "/static/") {
			return true
		}
	}
	if path == "/login" || path == "/signup" {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost:
			return true
		}
	}
	return false
}

func (a *authService) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	destination := loginRedirectURL(a.requestDestination(r))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", destination)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

// requestDestination returns a safe suffix for the login redirect. Only the
// application root, including its query string, is preserved because that is
// the sole full-page route; fragment endpoints always return to the root.
func (a *authService) requestDestination(r *http.Request) string {
	if r.Header.Get("HX-Request") == "true" {
		if destination, ok := sameOriginRootPath(r, r.Header.Get("HX-Current-URL")); ok {
			return destination
		}
		return "/"
	}
	return safeNextPath(r.URL.RequestURI())
}

// sameOriginRootPath validates an htmx-supplied current URL without trusting
// forwarded headers. The scheme is restricted to HTTP(S) so htmx's absolute
// location can be accepted behind a TLS-terminating proxy, while the host must
// match the request's Host header and only a root path (with query) survives.
func sameOriginRootPath(r *http.Request, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	if r.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
		return "", false
	}
	return safeNextPath(parsed.RequestURI()), true
}

// safeNextPath limits return destinations to the application root and its
// query string. It rejects absolute URLs, scheme-relative forms, backslashes,
// control characters, fragments, and any non-root path.
func safeNextPath(raw string) string {
	if raw == "" || len(raw) > authNextMaxBytes {
		return "/"
	}
	for _, character := range raw {
		if unicode.IsControl(character) || character == '\\' || character == '#' {
			return "/"
		}
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}

	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.IsAbs() || parsed.Opaque != "" || parsed.Host != "" || parsed.Scheme != "" || parsed.Path != "/" {
		return "/"
	}
	if parsed.RawQuery == "" {
		return "/"
	}
	return "/?" + parsed.RawQuery
}

func loginRedirectURL(next string) string {
	return "/login?next=" + url.QueryEscape(safeNextPath(next))
}

// sessionUser resolves the request's session cookie. It validates token shape
// before touching the database and hashes only after validation, so malformed
// or oversized values cannot cause storage work.
func (a *authService) sessionUser(r *http.Request) (kritui_db.User, bool, error) {
	cookie, err := r.Cookie(authSessionCookieName)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return kritui_db.User{}, false, nil
		}
		return kritui_db.User{}, false, nil
	}
	if !validSessionToken(cookie.Value) {
		return kritui_db.User{}, false, nil
	}

	user, err := kritui_db.GetSessionUser(r.Context(), a.database, hashSessionToken(cookie.Value), a.currentTime())
	if errors.Is(err, sql.ErrNoRows) {
		return kritui_db.User{}, false, nil
	}
	if err != nil {
		return kritui_db.User{}, false, err
	}
	return user, true, nil
}

func validSessionToken(raw string) bool {
	if len(raw) != base64.RawURLEncoding.EncodedLen(authSessionTokenBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == authSessionTokenBytes
}

func newSessionToken() (string, error) {
	token := make([]byte, authSessionTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func hashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *authService) currentTime() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

// rotateSession removes any previously presented valid session before
// creating a new one, so a successful sign-in cannot leave an older session
// usable alongside the new cookie.
func (a *authService) rotateSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	if cookie, err := r.Cookie(authSessionCookieName); err == nil && validSessionToken(cookie.Value) {
		if err := kritui_db.DeleteSession(r.Context(), a.database, hashSessionToken(cookie.Value)); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("revoke previous session: %w", err)
		}
	}

	raw, err := newSessionToken()
	if err != nil {
		return err
	}
	expires := a.currentTime().Add(authSessionLifetime)
	if err := kritui_db.CreateSession(r.Context(), a.database, userID, hashSessionToken(raw), expires); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	a.setSessionCookie(w, r, raw, expires)
	return nil
}

func (a *authService) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires.UTC(),
		MaxAge:   authSessionMaxAge,
		HttpOnly: true,
		Secure:   a.secureCookies || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *authService) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secureCookies || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// clientIP uses the transport peer address only. Forwarded headers are not
// consulted, so an untrusted client cannot reset or spread its rate bucket.
func clientIP(remoteAddr string) string {
	if address, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return address.Addr().Unmap().String()
	}
	if address, err := netip.ParseAddr(remoteAddr); err == nil {
		return address.Unmap().String()
	}
	trimmed := strings.TrimSpace(remoteAddr)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func (a *authService) allowAttempt(limiter *authRateLimiter, w http.ResponseWriter, r *http.Request) bool {
	allowed, retryAfter := limiter.allow(clientIP(r.RemoteAddr), a.currentTime())
	if allowed {
		return true
	}
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, "Too many attempts. Try again later.", http.StatusTooManyRequests)
	return false
}

type authRateLimitEntry struct {
	count int
	reset time.Time
}

// authRateLimiter is a fixed-window, per-IP limiter with a hard entry cap and
// stale-entry eviction. It prevents unbounded growth from spoofed or numerous
// client addresses.
type authRateLimiter struct {
	mu         sync.Mutex
	attempts   int
	window     time.Duration
	maxEntries int
	entries    map[string]authRateLimitEntry
}

func newAuthRateLimiter(attempts int, window time.Duration) *authRateLimiter {
	if attempts < 1 {
		attempts = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &authRateLimiter{
		attempts:   attempts,
		window:     window,
		maxEntries: authRateLimitMaxEntries,
		entries:    make(map[string]authRateLimitEntry),
	}
}

func (l *authRateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, found := l.entries[key]
	if !found {
		if len(l.entries) >= l.maxEntries {
			l.removeExpiredLocked(now)
		}
		if len(l.entries) >= l.maxEntries {
			l.removeOldestLocked()
		}
		l.entries[key] = authRateLimitEntry{count: 1, reset: now.Add(l.window)}
		return true, 0
	}

	if !now.Before(entry.reset) {
		entry = authRateLimitEntry{count: 0, reset: now.Add(l.window)}
	}
	if entry.count >= l.attempts {
		l.entries[key] = entry
		return false, entry.reset.Sub(now)
	}
	entry.count++
	l.entries[key] = entry
	return true, 0
}

func (l *authRateLimiter) removeExpiredLocked(now time.Time) {
	for key, entry := range l.entries {
		if !now.Before(entry.reset) {
			delete(l.entries, key)
		}
	}
}

func (l *authRateLimiter) removeOldestLocked() {
	var oldestKey string
	var oldestReset time.Time
	for key, entry := range l.entries {
		if oldestKey == "" || entry.reset.Before(oldestReset) {
			oldestKey = key
			oldestReset = entry.reset
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

// secureCookiesFromEnvironment reads KRITUI_SECURE_COOKIES. Secure cookies are
// the default; when the variable is set it must be exactly true or false.
func secureCookiesFromEnvironment() (bool, error) {
	value, present := os.LookupEnv("KRITUI_SECURE_COOKIES")
	if !present || strings.TrimSpace(value) == "" {
		return true, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("KRITUI_SECURE_COOKIES must be true or false")
	}
}
