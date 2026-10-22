package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	kritui_db "seesharpsi/kritui/db"
)

const authTestPassword = "correct horse battery"

func newAuthTestHandler(service *authService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "user=%d", kritui_db.UserID(r.Context()))
	})
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "history user=%d", kritui_db.UserID(r.Context()))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=123")
		_, _ = io.WriteString(w, "ok")
	})
	for _, path := range []string{"/static/app.css", "/sw.js", "/logo.png", "/favicon.ico"} {
		path := path
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=123")
			_, _ = io.WriteString(w, "asset")
		})
	}
	service.registerRoutes(mux)
	return http.NewCrossOriginProtection().Handler(service.middleware(mux))
}

func newAuthTestServer(t *testing.T, service *authService) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newAuthTestHandler(service))
	t.Cleanup(server.Close)
	return server
}

func newAuthTestClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func authDo(t *testing.T, client *http.Client, request *http.Request) (*http.Response, string) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform %s %s: %v", request.Method, request.URL, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read %s %s response: %v", request.Method, request.URL, err)
	}
	return response, string(body)
}

func authGet(t *testing.T, client *http.Client, target string, cookie *http.Cookie, headers map[string]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new GET request: %v", err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return authDo(t, client, request)
}

func authPostForm(t *testing.T, client *http.Client, target string, values url.Values, cookie *http.Cookie, headers map[string]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("new POST request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return authDo(t, client, request)
}

func authSessionCookie(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == authSessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response %d has no %s cookie", response.StatusCode, authSessionCookieName)
	return nil
}

func authSignup(t *testing.T, client *http.Client, serverURL, username, password, next string) (*http.Response, *http.Cookie) {
	t.Helper()
	values := url.Values{
		"username": {username},
		"password": {password},
	}
	if next != "" {
		values.Set("next", next)
	}
	response, _ := authPostForm(t, client, serverURL+"/signup", values, nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup status = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	return response, authSessionCookie(t, response)
}

func TestAuthSignupLoginLogoutRoundTrip(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	const username = "Alice_1"
	signupResponse, signupCookie := authSignup(t, client, server.URL, username, authTestPassword, "/?chat=7")
	if location := signupResponse.Header.Get("Location"); location != "/?chat=7" {
		t.Fatalf("signup Location = %q, want /?chat=7", location)
	}

	stored, err := kritui_db.GetUserByUsername(context.Background(), database, strings.ToLower(username))
	if err != nil {
		t.Fatalf("get signed-up user: %v", err)
	}
	if stored.ID == 1 {
		t.Fatal("public signup received the reserved owner ID")
	}
	if signupCookie.Secure {
		t.Fatal("direct HTTP local service set Secure on the session cookie")
	}
	if stored.PasswordHash == authTestPassword {
		t.Fatal("password was stored in plaintext")
	}
	if !verifyPassword(stored.PasswordHash, authTestPassword) {
		t.Fatal("stored password hash does not verify")
	}

	response, body := authGet(t, client, server.URL+"/", signupCookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated home status = %d, want 200", response.StatusCode)
	}
	if want := fmt.Sprintf("user=%d", stored.ID); body != want {
		t.Fatalf("authenticated home body = %q, want %q", body, want)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("private response Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if !strings.Contains(response.Header.Get("Vary"), "Cookie") {
		t.Errorf("private response Vary = %q, want Cookie", response.Header.Get("Vary"))
	}

	response, _ = authGet(t, client, server.URL+"/login", signupCookie, nil)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatalf("authenticated login page = %d %q, want 303 /", response.StatusCode, response.Header.Get("Location"))
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("authenticated login redirect Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("authenticated login redirect Referrer-Policy = %q, want no-referrer", response.Header.Get("Referrer-Policy"))
	}

	response, _ = authPostForm(t, client, server.URL+"/logout", url.Values{}, signupCookie, nil)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login" {
		t.Fatalf("logout = %d %q, want 303 /login", response.StatusCode, response.Header.Get("Location"))
	}
	cleared := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == authSessionCookieName && cookie.Value == "" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}

	response, _ = authGet(t, client, server.URL+"/", signupCookie, nil)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("revoked session home = %d %q, want 303 /login?next=%%2F", response.StatusCode, response.Header.Get("Location"))
	}

	loginValues := url.Values{
		"username": {username},
		"password": {authTestPassword},
		"next":     {"/?chat=7"},
	}
	loginResponse, _ := authPostForm(t, client, server.URL+"/login", loginValues, nil, nil)
	if loginResponse.StatusCode != http.StatusSeeOther || loginResponse.Header.Get("Location") != "/?chat=7" {
		t.Fatalf("login = %d %q, want 303 /?chat=7", loginResponse.StatusCode, loginResponse.Header.Get("Location"))
	}
	loginCookie := authSessionCookie(t, loginResponse)
	if loginCookie.Value == signupCookie.Value {
		t.Fatal("login reused the previous session token")
	}

	response, body = authGet(t, client, server.URL+"/", loginCookie, nil)
	if response.StatusCode != http.StatusOK || body != fmt.Sprintf("user=%d", stored.ID) {
		t.Fatalf("post-login home = %d %q, want authenticated user %d", response.StatusCode, body, stored.ID)
	}
}

func TestAuthSignupClaimsLegacySkrittleAccount(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	if _, err := database.Exec(`
		UPDATE settings SET default_model = 'legacy-model', llm_api_key = 'legacy-key', theme = 'nord'
		WHERE id = 1;
		INSERT INTO chats (id, title) VALUES (7, 'legacy chat');
		INSERT INTO messages (chat_id, position, role, content)
			VALUES (7, 0, 'user', 'legacy question');
	`); err != nil {
		t.Fatalf("seed legacy data: %v", err)
	}

	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	signupResponse, cookie := authSignup(t, client, server.URL, "SKRITTLE", authTestPassword, "/?chat=7")
	if location := signupResponse.Header.Get("Location"); location != "/?chat=7" {
		t.Fatalf("signup Location = %q, want /?chat=7", location)
	}

	stored, err := kritui_db.GetUserByUsername(ctx, database, "skrittle")
	if err != nil {
		t.Fatalf("get claimed legacy account: %v", err)
	}
	if stored.ID != kritui_db.LegacyOwnerID || stored.Username != kritui_db.LegacyOwnerUsername {
		t.Fatalf("claimed legacy account = %#v", stored)
	}
	if !verifyPassword(stored.PasswordHash, authTestPassword) {
		t.Fatal("claimed account password does not verify")
	}
	var disabled int
	if err := database.QueryRow(`SELECT disabled FROM users WHERE id = 1`).Scan(&disabled); err != nil || disabled != 0 {
		t.Fatalf("claimed account disabled = %d, %v, want enabled", disabled, err)
	}

	response, body := authGet(t, client, server.URL+"/", cookie, nil)
	if response.StatusCode != http.StatusOK || body != "user=1" {
		t.Fatalf("claimed account home = %d %q, want authenticated user 1", response.StatusCode, body)
	}
	ownerCtx := kritui_db.WithUser(ctx, kritui_db.LegacyOwnerID)
	if got, err := kritui_db.GetDefaultModel(ownerCtx, database, "fallback"); err != nil || got != "legacy-model" {
		t.Fatalf("claimed account model = %q, %v, want legacy-model", got, err)
	}
	if config, err := kritui_db.GetLLMConfig(ownerCtx, database); err != nil || config.APIKey != "legacy-key" {
		t.Fatalf("claimed account LLM key = %q, %v, want preserved secret", config.APIKey, err)
	}
	var chatOwner int64
	if err := database.QueryRow(`SELECT user_id FROM chats WHERE id = 7`).Scan(&chatOwner); err != nil || chatOwner != kritui_db.LegacyOwnerID {
		t.Fatalf("legacy chat owner = %d, %v, want 1", chatOwner, err)
	}
	var messageCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 7`).Scan(&messageCount); err != nil || messageCount != 1 {
		t.Fatalf("legacy message count = %d, %v, want 1", messageCount, err)
	}

	const replacementPassword = "replacement password value"
	duplicateValues := url.Values{
		"username": {"skrittle"},
		"password": {replacementPassword},
	}
	duplicateResponse, _ := authPostForm(t, client, server.URL+"/signup", duplicateValues, nil, nil)
	if duplicateResponse.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate signup status = %d, want %d", duplicateResponse.StatusCode, http.StatusConflict)
	}
	after, err := kritui_db.GetUserByUsername(ctx, database, "Skrittle")
	if err != nil {
		t.Fatalf("get account after duplicate signup: %v", err)
	}
	if after.PasswordHash != stored.PasswordHash {
		t.Fatal("duplicate signup reset the claimed password hash")
	}
	if !verifyPassword(after.PasswordHash, authTestPassword) {
		t.Fatal("original password no longer verifies")
	}
	if verifyPassword(after.PasswordHash, replacementPassword) {
		t.Fatal("duplicate signup applied a replacement password")
	}

	loginValues := url.Values{
		"username": {"skrittle"},
		"password": {authTestPassword},
	}
	loginResponse, _ := authPostForm(t, client, server.URL+"/login", loginValues, nil, nil)
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("claimed account login = %d, want %d", loginResponse.StatusCode, http.StatusSeeOther)
	}
	rejectedValues := url.Values{
		"username": {"skrittle"},
		"password": {replacementPassword},
	}
	rejectedResponse, _ := authPostForm(t, client, server.URL+"/login", rejectedValues, nil, nil)
	if rejectedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replacement password login = %d, want %d", rejectedResponse.StatusCode, http.StatusUnauthorized)
	}
}

func TestAuthLoginRotatesAndRevokesPreviousSession(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	_, firstCookie := authSignup(t, client, server.URL, "Rotate_User", authTestPassword, "")
	loginValues := url.Values{
		"username": {"Rotate_User"},
		"password": {authTestPassword},
	}
	response, _ := authPostForm(t, client, server.URL+"/login", loginValues, firstCookie, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", response.StatusCode)
	}
	secondCookie := authSessionCookie(t, response)
	if secondCookie.Value == firstCookie.Value {
		t.Fatal("rotated session kept the previous token")
	}

	response, _ = authGet(t, client, server.URL+"/", firstCookie, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("previous session status = %d, want 303", response.StatusCode)
	}
	response, _ = authGet(t, client, server.URL+"/", secondCookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rotated session status = %d, want 200", response.StatusCode)
	}
}

func TestAuthSessionCookieFlags(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, true)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	response, cookie := authSignup(t, client, server.URL, "Cookie_User", authTestPassword, "")
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("signup Cache-Control = %q, want no-store", got)
	}
	if cookie.Name != authSessionCookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, authSessionCookieName)
	}
	if cookie.Value == "" || !validSessionToken(cookie.Value) {
		t.Fatalf("cookie value %q is not a valid session token", cookie.Value)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
	if !cookie.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if !cookie.Secure {
		t.Error("cookie is not Secure with secure cookies configured")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Domain != "" {
		t.Errorf("cookie Domain = %q, want host-only", cookie.Domain)
	}
	if cookie.MaxAge != authSessionMaxAge {
		t.Errorf("cookie MaxAge = %d, want %d", cookie.MaxAge, authSessionMaxAge)
	}
	if cookie.Expires.IsZero() {
		t.Fatal("cookie has no persistent Expires value")
	}
	if remaining := time.Until(cookie.Expires); remaining < 29*24*time.Hour || remaining > 31*24*time.Hour {
		t.Errorf("cookie lifetime = %v, want about 30 days", remaining)
	}
}

func TestAuthMultipleSessionsRemainIndependent(t *testing.T) {
	database := openTestDatabase(t)
	encoded, err := hashPassword(authTestPassword)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	if _, err := kritui_db.CreateUser(context.Background(), database, "Multi_Session_User", encoded); err != nil {
		t.Fatalf("create multi-session user: %v", err)
	}

	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()
	values := url.Values{
		"username": {"Multi_Session_User"},
		"password": {authTestPassword},
	}

	firstResponse, _ := authPostForm(t, client, server.URL+"/login", values, nil, nil)
	firstCookie := authSessionCookie(t, firstResponse)
	secondResponse, _ := authPostForm(t, client, server.URL+"/login", values, nil, nil)
	secondCookie := authSessionCookie(t, secondResponse)
	if firstCookie.Value == secondCookie.Value {
		t.Fatal("separate logins reused one session token")
	}

	response, _ := authPostForm(t, client, server.URL+"/logout", url.Values{}, firstCookie, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", response.StatusCode)
	}
	response, _ = authGet(t, client, server.URL+"/", firstCookie, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("logged-out session status = %d, want 303", response.StatusCode)
	}
	response, _ = authGet(t, client, server.URL+"/", secondCookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("independent session status = %d, want 200", response.StatusCode)
	}
}

func TestAuthCookieSecureFromTLS(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	handler := newAuthTestHandler(service)

	values := url.Values{
		"username": {"TLS_User"},
		"password": {authTestPassword},
	}
	request := httptest.NewRequest(http.MethodPost, "https://kritui.example/signup", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.TLS = &tls.ConnectionState{}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	response := recorder.Result()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup over TLS status = %d, want 303", response.StatusCode)
	}
	cookie := authSessionCookie(t, response)
	if !cookie.Secure {
		t.Fatal("direct TLS request did not receive a Secure cookie")
	}
}

func TestAuthStoresOnlySessionTokenHash(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	_, cookie := authSignup(t, client, server.URL, "Hash_User", authTestPassword, "")
	rawToken := cookie.Value
	if len(rawToken) != 43 {
		t.Fatalf("raw token length = %d, want 43", len(rawToken))
	}
	stored, err := kritui_db.GetUserByUsername(context.Background(), database, "Hash_User")
	if err != nil {
		t.Fatalf("get hashed-session user: %v", err)
	}

	user, err := kritui_db.GetSessionUser(context.Background(), database, hashSessionToken(rawToken), time.Now())
	if err != nil {
		t.Fatalf("look up session by token hash: %v", err)
	}
	if user.ID != stored.ID {
		t.Fatalf("session user ID = %d, want %d", user.ID, stored.ID)
	}
	if hashSessionToken(rawToken) == rawToken {
		t.Fatal("session hash equals the raw token")
	}
	if _, err := kritui_db.GetSessionUser(context.Background(), database, rawToken, time.Now()); err == nil {
		t.Fatal("session lookup accepted the raw token instead of its hash")
	}
}

func TestAuthUsernameMatchingIsCaseInsensitive(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()
	ctx := context.Background()

	authSignup(t, client, server.URL, "Case_User", authTestPassword, "")
	stored, err := kritui_db.GetUserByUsername(ctx, database, "CASE_USER")
	if err != nil {
		t.Fatalf("get canonical user: %v", err)
	}

	loginValues := url.Values{
		"username": {"cAsE_uSeR"},
		"password": {authTestPassword},
	}
	response, _ := authPostForm(t, client, server.URL+"/login", loginValues, nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("case-varied login status = %d, want 303", response.StatusCode)
	}
	cookie := authSessionCookie(t, response)
	response, body := authGet(t, client, server.URL+"/", cookie, nil)
	if response.StatusCode != http.StatusOK || body != fmt.Sprintf("user=%d", stored.ID) {
		t.Fatalf("case-varied login home = %d %q, want user %d", response.StatusCode, body, stored.ID)
	}

	duplicate := url.Values{
		"username": {"case_user"},
		"password": {authTestPassword},
	}
	response, body = authPostForm(t, client, server.URL+"/signup", duplicate, nil, nil)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("case-varied duplicate signup status = %d, want 409", response.StatusCode)
	}
	if !strings.Contains(body, authUsernameTakenMessage) {
		t.Fatalf("duplicate signup body %q lacks %q", body, authUsernameTakenMessage)
	}
}

func TestAuthBadPasswordsUseGenericError(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()
	const username = "Bad_Pass_User"
	const wrongPassword = "definitely wrong password"

	authSignup(t, client, server.URL, username, authTestPassword, "")

	tests := []struct {
		name     string
		username string
		password string
		status   int
	}{
		{name: "wrong password", username: username, password: wrongPassword, status: http.StatusUnauthorized},
		{name: "unknown username", username: "Never_Exists_User", password: wrongPassword, status: http.StatusUnauthorized},
		{name: "missing password", username: username, password: "", status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := url.Values{
				"username": {test.username},
				"password": {test.password},
				"next":     {"/?chat=3"},
			}
			response, body := authPostForm(t, client, server.URL+"/login", values, nil, nil)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if strings.Contains(body, wrongPassword) {
				t.Fatal("login page echoed the submitted password")
			}
			if test.status == http.StatusUnauthorized {
				if !strings.Contains(body, authInvalidCredentialsMessage) {
					t.Fatalf("login body %q lacks generic error %q", body, authInvalidCredentialsMessage)
				}
				for _, cookie := range response.Cookies() {
					if cookie.Name == authSessionCookieName && cookie.Value != "" {
						t.Fatal("failed login created a session cookie")
					}
				}
			} else if !strings.Contains(body, authMissingCredentialsMessage) {
				t.Fatalf("missing-password body %q lacks %q", body, authMissingCredentialsMessage)
			}
			if !strings.Contains(body, `value="/?chat=3"`) {
				t.Errorf("login page did not preserve next destination")
			}
		})
	}
}

func TestAuthSessionExpiryRejected(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	_, cookie := authSignup(t, client, server.URL, "Expiry_User", authTestPassword, "")
	service.now = func() time.Time {
		return time.Now().Add(authSessionLifetime + time.Minute)
	}

	response, _ := authGet(t, client, server.URL+"/", cookie, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("expired session status = %d, want 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "/login?next=%2F" {
		t.Fatalf("expired session Location = %q, want /login?next=%%2F", location)
	}
}

func TestAuthUnauthenticatedNavigationAndHTMX(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	response, _ := authGet(t, client, server.URL+"/?chat=4", nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("navigation status = %d, want 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "/login?next=%2F%3Fchat%3D4" {
		t.Fatalf("navigation Location = %q, want /login?next=%%2F%%3Fchat%%3D4", location)
	}

	response, body := authGet(t, client, server.URL+"/?chat=4", nil, map[string]string{
		"HX-Request":      "true",
		"HX-Current-URL":  server.URL + "/?chat=4",
		"HX-Request-Type": "partial",
	})
	if response.StatusCode != http.StatusNoContent || body != "" {
		t.Fatalf("htmx navigation = %d %q, want 204 with empty body", response.StatusCode, body)
	}
	if redirect := response.Header.Get("HX-Redirect"); redirect != "/login?next=%2F%3Fchat%3D4" {
		t.Fatalf("htmx HX-Redirect = %q, want /login?next=%%2F%%3Fchat%%3D4", redirect)
	}
	if response.Header.Get("Location") != "" {
		t.Fatal("htmx navigation returned a Location header")
	}

	response, _ = authGet(t, client, server.URL+"/", nil, map[string]string{
		"HX-Request":     "true",
		"HX-Current-URL": "https://evil.example/?chat=4",
	})
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("cross-origin htmx current URL status = %d, want 204", response.StatusCode)
	}
	if redirect := response.Header.Get("HX-Redirect"); redirect != "/login?next=%2F" {
		t.Fatalf("cross-origin htmx HX-Redirect = %q, want /login?next=%%2F", redirect)
	}

	response, _ = authGet(t, client, server.URL+"/settings", nil, map[string]string{
		"HX-Request":     "true",
		"HX-Current-URL": server.URL + "/settings?tab=chat",
	})
	if redirect := response.Header.Get("HX-Redirect"); redirect != "/login?next=%2F" {
		t.Fatalf("non-root htmx HX-Redirect = %q, want /login?next=%%2F", redirect)
	}

	response, _ = authGet(t, client, server.URL+"/messages/tools", nil, map[string]string{
		"Accept": "text/event-stream",
	})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("event stream status = %d, want 401", response.StatusCode)
	}
	if response.Header.Get("Location") != "" {
		t.Fatal("event stream returned a redirect")
	}
}

func TestAuthPagesSanitizeReturnDestination(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	response, body := authGet(t, client, server.URL+"/login?next="+url.QueryEscape("/?chat=2"), nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login page status = %d, want 200", response.StatusCode)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("login page Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("login page Referrer-Policy = %q, want no-referrer", response.Header.Get("Referrer-Policy"))
	}
	if !strings.Contains(body, `name="next"`) || !strings.Contains(body, `value="/?chat=2"`) {
		t.Fatalf("login page did not preserve safe next: %q", body)
	}

	response, body = authGet(t, client, server.URL+"/signup?next="+url.QueryEscape("https://evil.example/steal"), nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signup page status = %d, want 200", response.StatusCode)
	}
	if strings.Contains(body, "evil.example") {
		t.Fatal("signup page reflected an unsafe absolute return destination")
	}
	if !strings.Contains(body, `value="/"`) {
		t.Fatal("signup page did not fall back to the root return destination")
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("signup page headers = Cache-Control %q, Referrer-Policy %q", response.Header.Get("Cache-Control"), response.Header.Get("Referrer-Policy"))
	}
}

func TestAuthRateLimitsLoginAndSignup(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	encoded, err := hashPassword(authTestPassword)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	if _, err := kritui_db.CreateUser(ctx, database, "Rate_User", encoded); err != nil {
		t.Fatalf("create rate-limit user: %v", err)
	}

	loginService := newAuthService(database, false)
	loginService.loginLimiter = newAuthRateLimiter(2, time.Minute)
	loginServer := newAuthTestServer(t, loginService)
	loginClient := newAuthTestClient()

	values := url.Values{
		"username": {"Rate_User"},
		"password": {"wrong password value"},
	}
	for attempt := 0; attempt < 2; attempt++ {
		response, _ := authPostForm(t, loginClient, loginServer.URL+"/login", values, nil, map[string]string{
			"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", attempt+1),
		})
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt+1, response.StatusCode)
		}
	}
	response, body := authPostForm(t, loginClient, loginServer.URL+"/login", values, nil, map[string]string{
		"X-Forwarded-For": "198.51.100.25",
	})
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited login status = %d, want 429", response.StatusCode)
	}
	retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After"))
	if err != nil || retryAfter < 1 || retryAfter > int(authRateLimitWindow.Seconds()) {
		t.Fatalf("Retry-After = %q, want 1..%d seconds", response.Header.Get("Retry-After"), int(authRateLimitWindow.Seconds()))
	}
	if !strings.Contains(body, "Too many attempts") {
		t.Fatalf("rate-limited body %q lacks generic message", body)
	}

	signupService := newAuthService(database, false)
	signupService.signupLimiter = newAuthRateLimiter(1, time.Minute)
	signupServer := newAuthTestServer(t, signupService)
	signupValues := url.Values{
		"username": {"x"},
		"password": {authTestPassword},
	}
	response, _ = authPostForm(t, loginClient, signupServer.URL+"/signup", signupValues, nil, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("first signup status = %d, want 400", response.StatusCode)
	}
	response, _ = authPostForm(t, loginClient, signupServer.URL+"/signup", signupValues, nil, nil)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited signup status = %d, want 429", response.StatusCode)
	}
}

func TestAuthCrossOriginRequestsRejected(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	values := url.Values{
		"username": {"Cross_Origin_User"},
		"password": {authTestPassword},
	}
	response, _ := authPostForm(t, client, server.URL+"/signup", values, nil, map[string]string{
		"Origin":         "https://evil.example",
		"Sec-Fetch-Site": "cross-site",
	})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin signup status = %d, want 403", response.StatusCode)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == authSessionCookieName && cookie.Value != "" {
			t.Fatal("cross-origin signup created a session cookie")
		}
	}
	if _, err := kritui_db.GetUserByUsername(context.Background(), database, "Cross_Origin_User"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-origin signup user lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestAuthPublicAssetsAndHealthRemainPublic(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	for _, path := range []string{"/static/app.css?v=1", "/sw.js", "/logo.png", "/favicon.ico", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			response, body := authGet(t, client, server.URL+path, nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if body == "" {
				t.Fatal("public response body is empty")
			}
			if response.Header.Get("Cache-Control") != "public, max-age=123" {
				t.Errorf("Cache-Control = %q, want handler value untouched", response.Header.Get("Cache-Control"))
			}
			if strings.Contains(response.Header.Get("Vary"), "Cookie") {
				t.Errorf("public response Vary = %q, want no Cookie", response.Header.Get("Vary"))
			}
		})
	}
}

func TestAuthDatabaseFailureReturnsGeneric500(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	_, cookie := authSignup(t, client, server.URL, "DB_Failure_User", authTestPassword, "")
	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	malformed := &http.Cookie{Name: authSessionCookieName, Value: strings.Repeat("a", 1000)}
	response, _ := authGet(t, client, server.URL+"/", malformed, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("malformed token status = %d, want 303 without a database lookup", response.StatusCode)
	}

	response, body := authGet(t, client, server.URL+"/", cookie, nil)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("database failure status = %d, want 500", response.StatusCode)
	}
	if body != "Internal Server Error\n" {
		t.Fatalf("database failure body = %q, want a generic error", body)
	}
}

func TestAuthFormBodyLimits(t *testing.T) {
	database := openTestDatabase(t)
	service := newAuthService(database, false)
	server := newAuthTestServer(t, service)
	client := newAuthTestClient()

	oversized := "username=" + strings.Repeat("a", authFormMaxBytes+1) + "&password=value"
	request, err := http.NewRequest(http.MethodPost, server.URL+"/login", strings.NewReader(oversized))
	if err != nil {
		t.Fatalf("new oversized request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, _ := authDo(t, client, request)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form status = %d, want 413", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/login", strings.NewReader("%zz"))
	if err != nil {
		t.Fatalf("new malformed request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, _ = authDo(t, client, request)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed form status = %d, want 400", response.StatusCode)
	}
}
