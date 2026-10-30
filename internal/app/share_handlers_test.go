package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/themes"
)

var (
	shareFragmentTokenPattern    = regexp.MustCompile(`data-share-url="/s/([A-Za-z0-9_-]{43})"`)
	sharePreparedTokenPattern    = regexp.MustCompile(`data-share-token="([A-Za-z0-9_-]{43})"`)
	sharePreparedSnapshotPattern = regexp.MustCompile(`<template data-share-snapshot>([A-Za-z0-9_-]+)</template>`)
)

func newShareTestServer(t *testing.T, database *sql.DB) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		newToolCallStore(),
		nil,
		newAuthService(database, false),
	))
	t.Cleanup(server.Close)
	return server
}

func shareTestUserID(t *testing.T, database *sql.DB, username string) int64 {
	t.Helper()
	var userID int64
	if err := database.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&userID); err != nil {
		t.Fatalf("look up share test user %q: %v", username, err)
	}
	return userID
}

func seedShareChat(t *testing.T, database *sql.DB, userID int64, title string) int64 {
	t.Helper()
	var chatID int64
	if err := database.QueryRow(`
		INSERT INTO chats (user_id, title) VALUES (?, ?) RETURNING id
	`, userID, title).Scan(&chatID); err != nil {
		t.Fatalf("insert share chat: %v", err)
	}
	return chatID
}

func seedShareMessage(t *testing.T, database *sql.DB, chatID int64, position int, role, content string) int64 {
	t.Helper()
	var messageID int64
	if err := database.QueryRow(`
		INSERT INTO messages (chat_id, position, role, content)
		VALUES (?, ?, ?, ?) RETURNING id
	`, chatID, position, role, content).Scan(&messageID); err != nil {
		t.Fatalf("insert share message: %v", err)
	}
	return messageID
}

func shareTestHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func shareTokenFromFragment(t *testing.T, fragment string) string {
	t.Helper()
	matches := shareFragmentTokenPattern.FindStringSubmatch(fragment)
	if len(matches) != 2 {
		t.Fatalf("share fragment has no canonical token: %s", fragment)
	}
	return matches[1]
}

func sharePreparedValues(t *testing.T, fragment string) (string, string) {
	t.Helper()
	tokenMatches := sharePreparedTokenPattern.FindStringSubmatch(fragment)
	snapshotMatches := sharePreparedSnapshotPattern.FindStringSubmatch(fragment)
	if len(tokenMatches) != 2 || len(snapshotMatches) != 2 {
		t.Fatalf("prepared share fragment lacks a token or snapshot: %s", fragment)
	}
	return tokenMatches[1], snapshotMatches[1]
}

func shareDecodedSnapshot(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode prepared snapshot: %v", err)
	}
	return string(raw)
}

func shareTarget(serverURL string, chatID, messageID int64) string {
	return fmt.Sprintf("%s/chats/%d/messages/%d/share", serverURL, chatID, messageID)
}

func sharePublishTarget(serverURL string, chatID, messageID int64) string {
	return shareTarget(serverURL, chatID, messageID) + "/publish"
}

func prepareShare(t *testing.T, client *http.Client, serverURL string, cookie *http.Cookie, chatID, messageID int64) (string, string, string) {
	t.Helper()
	response, fragment := authPostForm(t, client, shareTarget(serverURL, chatID, messageID), url.Values{}, cookie, map[string]string{
		"HX-Request": "true",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare share status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, fragment)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("prepare Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("prepare X-Content-Type-Options = %q, want nosniff", response.Header.Get("X-Content-Type-Options"))
	}
	token, snapshot := sharePreparedValues(t, fragment)
	return token, snapshot, fragment
}

func newShareKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate share key: %v", err)
	}
	return key
}

func encryptShare(t *testing.T, key, plaintext []byte) ([]byte, []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create share cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create share GCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate share nonce: %v", err)
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil)
}

func decryptShare(t *testing.T, key, nonce, ciphertext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create share cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create share GCM: %v", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt stored share: %v", err)
	}
	return plaintext
}

func sharePublishValues(token string, nonce, ciphertext []byte) url.Values {
	return url.Values{
		"token":          {token},
		"format_version": {strconv.Itoa(kritui_db.SharedResponseFormatVersion)},
		"nonce":          {base64.RawURLEncoding.EncodeToString(nonce)},
		"ciphertext":     {base64.RawURLEncoding.EncodeToString(ciphertext)},
	}
}

func cloneShareValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for name, entries := range values {
		clone[name] = append([]string(nil), entries...)
	}
	return clone
}

func shareRowCount(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM shared_responses`).Scan(&count); err != nil {
		t.Fatalf("count shared responses: %v", err)
	}
	return count
}

func shareStoredResponse(t *testing.T, database *sql.DB, token string) kritui_db.SharedResponse {
	t.Helper()
	share, err := kritui_db.GetSharedResponse(context.Background(), database, shareTestHash(token))
	if err != nil {
		t.Fatalf("read stored shared response: %v", err)
	}
	return share
}

func TestMessageSharePrepareReturnsTransientSanitizedSnapshot(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	if _, err := database.Exec(`UPDATE settings SET theme = 'nord' WHERE id = ?`, userID); err != nil {
		t.Fatalf("set share owner theme: %v", err)
	}
	chatID := seedShareChat(t, database, userID, "Shared <title>")
	const prompt = "Share prompt <b>bold</b> & text"
	seedShareMessage(t, database, chatID, 0, "user", prompt)
	messageID := seedShareMessage(t, database, chatID, 1, "assistant", strings.Join([]string{
		"# Shared answer",
		"",
		"Safe **bold** text and `code`.",
		"",
		"<script>alert('bad')</script>",
		"",
		`<img src="x" onerror="alert(1)">`,
		"",
		"[unsafe](javascript:alert(1))",
	}, "\n"))

	token, encodedSnapshot, fragment := prepareShare(t, client, server.URL, cookie, chatID, messageID)
	if len(token) != sharedTokenLength {
		t.Fatalf("prepared token length = %d, want %d", len(token), sharedTokenLength)
	}
	requireContains(t, fragment, "data-share-preparation", "data-share-token="+`"`+token+`"`, "<template data-share-snapshot>")

	snapshot := shareDecodedSnapshot(t, encodedSnapshot)
	if !strings.HasPrefix(strings.ToLower(snapshot), "<!doctype html>") {
		t.Errorf("prepared snapshot is not a complete HTML document: %q", snapshot)
	}
	requireContains(t, snapshot,
		`data-theme="nord"`,
		`--color-background:#`,
		`color-scheme:dark`,
		"Shared answer",
		"<strong>bold</strong>",
		"Share prompt &lt;b&gt;bold&lt;/b&gt; &amp; text",
	)
	requireNotContains(t, snapshot,
		"<script",
		"javascript:",
		"onerror",
		"<link",
		"/static/",
		"@font-face",
		`Share prompt <b>bold</b> & text`,
	)
	if count := shareRowCount(t, database); count != 0 {
		t.Fatalf("prepare stored %d rows, want 0", count)
	}
}

func TestMessageSharePrepareUsesSelectedOptionsAndWholeConversation(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()
	const username = "Share_Options_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	chatID := seedShareChat(t, database, userID, "Original title")
	seedShareMessage(t, database, chatID, 0, "user", "Earlier question")
	seedShareMessage(t, database, chatID, 1, "assistant", "Earlier answer")
	toolCallID := seedShareMessage(t, database, chatID, 2, "assistant", "")
	if _, err := database.Exec(`
		INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
		VALUES (?, 0, 'call-private', 'function', 'private', '{"secret":"arguments"}')
	`, toolCallID); err != nil {
		t.Fatalf("insert private tool history: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO messages (chat_id, position, role, content, tool_call_id) VALUES (?, 3, 'tool', 'private tool output', 'call-private')`, chatID); err != nil {
		t.Fatalf("insert tool result: %v", err)
	}
	hiddenID := seedShareMessage(t, database, chatID, 4, "user", "Hidden prompt")
	if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = ?`, hiddenID); err != nil {
		t.Fatalf("hide prompt: %v", err)
	}
	messageID := seedShareMessage(t, database, chatID, 5, "assistant", "Current answer")

	values := url.Values{
		"title":    {"Custom share title"},
		"theme":    {"1975"},
		"mode":     {"whole"},
		"markdown": {"Edited response should not replace whole chat"},
	}
	response, fragment := authPostForm(t, client, shareTarget(server.URL, chatID, messageID), values, cookie, map[string]string{"HX-Request": "true"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("whole-chat prepare status = %d; body = %q", response.StatusCode, fragment)
	}
	_, encoded := sharePreparedValues(t, fragment)
	snapshot := shareDecodedSnapshot(t, encoded)
	requireContains(t, snapshot,
		`data-theme="1975"`,
		"Custom share title",
		"<h2>User</h2>", "Earlier question",
		"<h2>Assistant</h2>", "Earlier answer", "Current answer",
	)
	requireNotContains(t, snapshot,
		"Original title", "Edited response should not replace whole chat",
		"private tool", "secret", "Hidden prompt",
	)
}

func TestParseShareOptionsRejectsInvalidValuesAndKeepsLegacyDefaults(t *testing.T) {
	source := kritui_db.ShareSource{Title: "Stored title", UserContent: "Prompt", Content: "Stored Markdown"}
	defaultTheme := themes.Default()
	title, content, mode, theme, status := parseShareOptions(nil, source, defaultTheme)
	if status != 0 || title != "Stored title" || content != "Stored Markdown" || mode != "recent" || theme.ID != defaultTheme.ID {
		t.Fatalf("legacy defaults = (%q, %q, %q, %q, %d)", title, content, mode, theme.ID, status)
	}
	for _, values := range []map[string][]string{
		{"title": {"title"}, "theme": {"nope"}, "mode": {"recent"}, "markdown": {"ok"}},
		{"title": {"title"}, "theme": {"nord"}, "mode": {"unknown"}, "markdown": {"ok"}},
		{"title": {" "}, "theme": {"nord"}, "mode": {"recent"}, "markdown": {"ok"}},
		{"title": {"title", "duplicate"}, "theme": {"nord"}, "mode": {"recent"}, "markdown": {"ok"}},
		{"title": {strings.Repeat("x", maxChatTitleRunes+1)}, "theme": {"nord"}, "mode": {"recent"}, "markdown": {"ok"}},
		{"title": {"title"}, "theme": {"nord"}, "mode": {"recent"}, "markdown": {strings.Repeat("x", maxShareMarkdownBytes+1)}},
	} {
		if _, _, _, _, status := parseShareOptions(values, source, defaultTheme); status != http.StatusBadRequest {
			t.Errorf("parseShareOptions(%v) status = %d, want 400", values, status)
		}
	}
}

func TestMessageSharePrepareRequiresHTMX(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_No_JS_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	chatID := seedShareChat(t, database, userID, "No script chat")
	messageID := seedShareMessage(t, database, chatID, 0, "assistant", "No script answer")

	response, body := authPostForm(t, client, shareTarget(server.URL, chatID, messageID), url.Values{}, cookie, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-htmx prepare status = %d, want %d; body = %q", response.StatusCode, http.StatusBadRequest, body)
	}
	if location := response.Header.Get("Location"); location != "" {
		t.Errorf("non-htmx prepare Location = %q, want empty", location)
	}
	requireContains(t, strings.ToLower(body), "javascript")
	if count := shareRowCount(t, database); count != 0 {
		t.Fatalf("non-htmx prepare stored %d rows, want 0", count)
	}
}

func TestMessageSharePrepareAndPublishRejectForeignHiddenAndCrossOrigin(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const ownerName = "Share_Security_Owner"
	_, ownerCookie := authSignup(t, client, server.URL, ownerName, authTestPassword, "")
	ownerID := shareTestUserID(t, database, ownerName)

	const foreignName = "Share_Security_Foreign"
	_, foreignCookie := authSignup(t, client, server.URL, foreignName, authTestPassword, "")
	foreignID := shareTestUserID(t, database, foreignName)

	ownerChatID := seedShareChat(t, database, ownerID, "Owner chat")
	shareableID := seedShareMessage(t, database, ownerChatID, 0, "assistant", "Shareable answer")
	userMessageID := seedShareMessage(t, database, ownerChatID, 1, "user", "User question")
	hiddenID := seedShareMessage(t, database, ownerChatID, 2, "assistant", "Hidden answer")
	if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = ?`, hiddenID); err != nil {
		t.Fatalf("hide owner message: %v", err)
	}
	toolCallID := seedShareMessage(t, database, ownerChatID, 3, "assistant", "Tool answer")
	if _, err := database.Exec(`
		INSERT INTO message_tool_calls (message_id, position, call_id, call_type, function_name, arguments)
		VALUES (?, 0, 'call-1', 'function', 'lookup', '{}')
	`, toolCallID); err != nil {
		t.Fatalf("insert owner tool call: %v", err)
	}
	publishableID := seedShareMessage(t, database, ownerChatID, 4, "assistant", "Publishable answer")
	publishHiddenID := seedShareMessage(t, database, ownerChatID, 5, "assistant", "Publish then hide")

	crossChatID := seedShareChat(t, database, ownerID, "Owner other chat")
	crossChatMessageID := seedShareMessage(t, database, crossChatID, 0, "assistant", "Other chat answer")

	foreignChatID := seedShareChat(t, database, foreignID, "Foreign chat")
	foreignMessageID := seedShareMessage(t, database, foreignChatID, 0, "assistant", "Foreign answer")

	cases := []struct {
		name      string
		cookie    *http.Cookie
		chatID    int64
		messageID int64
	}{
		{name: "foreign session", cookie: foreignCookie, chatID: ownerChatID, messageID: shareableID},
		{name: "user message", cookie: ownerCookie, chatID: ownerChatID, messageID: userMessageID},
		{name: "hidden assistant", cookie: ownerCookie, chatID: ownerChatID, messageID: hiddenID},
		{name: "tool call assistant", cookie: ownerCookie, chatID: ownerChatID, messageID: toolCallID},
		{name: "cross-chat message ID", cookie: ownerCookie, chatID: ownerChatID, messageID: crossChatMessageID},
		{name: "foreign chat", cookie: ownerCookie, chatID: foreignChatID, messageID: foreignMessageID},
		{name: "missing message", cookie: ownerCookie, chatID: ownerChatID, messageID: 999999},
	}
	for _, testCase := range cases {
		t.Run("prepare "+testCase.name, func(t *testing.T) {
			response, body := authPostForm(t, client, shareTarget(server.URL, testCase.chatID, testCase.messageID), url.Values{}, testCase.cookie, map[string]string{
				"HX-Request": "true",
			})
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("prepare status = %d, want %d; body = %q", response.StatusCode, http.StatusNotFound, body)
			}
		})
	}

	publishToken, _, _ := prepareShare(t, client, server.URL, ownerCookie, ownerChatID, publishableID)
	publishValues := sharePublishValues(publishToken, make([]byte, 12), make([]byte, 16))
	publishableTarget := sharePublishTarget(server.URL, ownerChatID, publishableID)

	hiddenPublishToken, _, _ := prepareShare(t, client, server.URL, ownerCookie, ownerChatID, publishHiddenID)
	if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = ?`, publishHiddenID); err != nil {
		t.Fatalf("hide publish source: %v", err)
	}

	publishCases := []struct {
		name    string
		target  string
		values  url.Values
		cookie  *http.Cookie
		headers map[string]string
		want    int
	}{
		{name: "foreign session", target: publishableTarget, values: publishValues, cookie: foreignCookie, want: http.StatusNotFound},
		{name: "hidden source", target: sharePublishTarget(server.URL, ownerChatID, publishHiddenID), values: sharePublishValues(hiddenPublishToken, make([]byte, 12), make([]byte, 16)), cookie: ownerCookie, want: http.StatusNotFound},
		{name: "anonymous", target: publishableTarget, values: publishValues, cookie: nil, want: http.StatusSeeOther},
		{
			name:    "cross-origin",
			target:  publishableTarget,
			values:  publishValues,
			cookie:  ownerCookie,
			headers: map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			want:    http.StatusForbidden,
		},
	}
	for _, testCase := range publishCases {
		t.Run("publish "+testCase.name, func(t *testing.T) {
			response, body := authPostForm(t, client, testCase.target, testCase.values, testCase.cookie, testCase.headers)
			if response.StatusCode != testCase.want {
				t.Fatalf("publish status = %d, want %d; body = %q", response.StatusCode, testCase.want, body)
			}
			if testCase.want == http.StatusSeeOther {
				if location := response.Header.Get("Location"); location != "/login?next=%2F" {
					t.Errorf("anonymous publish Location = %q, want /login?next=%%2F", location)
				}
			}
		})
	}

	response, body := authPostForm(t, client, shareTarget(server.URL, ownerChatID, shareableID), url.Values{}, nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous prepare status = %d, want %d; body = %q", response.StatusCode, http.StatusSeeOther, body)
	}
	if location := response.Header.Get("Location"); location != "/login?next=%2F" {
		t.Errorf("anonymous prepare Location = %q, want /login?next=%%2F", location)
	}

	response, body = authPostForm(t, client, shareTarget(server.URL, ownerChatID, shareableID), url.Values{}, ownerCookie, map[string]string{
		"Origin":         "https://evil.example",
		"Sec-Fetch-Site": "cross-site",
	})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin prepare status = %d, want %d; body = %q", response.StatusCode, http.StatusForbidden, body)
	}

	if count := shareRowCount(t, database); count != 0 {
		t.Fatalf("rejected share requests stored %d rows, want 0", count)
	}
}

func TestMessageSharePublishStoresCiphertextAndServesBootstrap(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_Publish_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	if _, err := database.Exec(`UPDATE settings SET theme = 'nord' WHERE id = ?`, userID); err != nil {
		t.Fatalf("set publish owner theme: %v", err)
	}
	const title = "Frozen title"
	const prompt = "Frozen prompt <em>plain</em> & safe"
	const answer = "# Frozen answer\n\nFrozen body."
	chatID := seedShareChat(t, database, userID, title)
	seedShareMessage(t, database, chatID, 0, "user", prompt)
	messageID := seedShareMessage(t, database, chatID, 1, "assistant", answer)

	token, encodedSnapshot, _ := prepareShare(t, client, server.URL, cookie, chatID, messageID)
	snapshot := shareDecodedSnapshot(t, encodedSnapshot)
	requireContains(t, snapshot, `data-theme="nord"`, "Frozen prompt &lt;em&gt;plain&lt;/em&gt; &amp; safe", "Frozen answer")
	if count := shareRowCount(t, database); count != 0 {
		t.Fatalf("prepare stored %d rows, want 0", count)
	}

	key := newShareKey(t)
	nonce, ciphertext := encryptShare(t, key, []byte(snapshot))
	values := sharePublishValues(token, nonce, ciphertext)
	response, fragment := authPostForm(t, client, sharePublishTarget(server.URL, chatID, messageID), values, cookie, map[string]string{
		"HX-Request": "true",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, fragment)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("publish Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("publish X-Content-Type-Options = %q, want nosniff", response.Header.Get("X-Content-Type-Options"))
	}
	sharePath := "/s/" + token
	requireContains(t, fragment,
		`class="message-share-actions"`,
		`message-share-copy-button`,
		`data-share-url="`+sharePath+`"`,
		`class="message-share-status"`,
		`role="status"`,
	)
	requireNotContains(t, fragment, sharePath+"#")

	stored := shareStoredResponse(t, database, token)
	if stored.TokenHash != shareTestHash(token) {
		t.Errorf("stored token hash = %q, want SHA256 hex", stored.TokenHash)
	}
	if stored.ChatID != chatID || stored.MessageID != messageID {
		t.Errorf("stored share source = %d/%d, want %d/%d", stored.ChatID, stored.MessageID, chatID, messageID)
	}
	if stored.FormatVersion != kritui_db.SharedResponseFormatVersion {
		t.Errorf("stored format version = %d, want %d", stored.FormatVersion, kritui_db.SharedResponseFormatVersion)
	}
	if !bytes.Equal(stored.Nonce, nonce) {
		t.Error("stored nonce differs from the published nonce")
	}
	if !bytes.Equal(stored.Ciphertext, ciphertext) {
		t.Error("stored ciphertext differs from the published ciphertext")
	}
	if plaintext := decryptShare(t, key, stored.Nonce, stored.Ciphertext); string(plaintext) != snapshot {
		t.Error("stored ciphertext does not decrypt to the prepared snapshot")
	}

	assertSharedResponseColumnsAreCiphertextOnly(t, database)

	publicPath := server.URL + sharePath
	response, publicBody := authGet(t, client, publicPath, nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public bootstrap status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, publicBody)
	}
	assertSharedResponseHeaders(t, response)
	assertSharedResponseCSP(t, response)
	if response.ContentLength != int64(len(publicBody)) {
		t.Errorf("GET Content-Length = %d, want %d", response.ContentLength, len(publicBody))
	}
	requireContains(t, publicBody,
		`data-format-version="1"`,
		`data-nonce="`+base64.RawURLEncoding.EncodeToString(nonce)+`"`,
		`data-ciphertext="`+base64.RawURLEncoding.EncodeToString(ciphertext)+`"`,
		`sandbox="allow-popups allow-popups-to-escape-sandbox"`,
	)
	requireNotContains(t, publicBody,
		title,
		"Frozen prompt",
		"Frozen answer",
		"Frozen body",
		base64.RawURLEncoding.EncodeToString(key),
		"data-theme=",
	)
	requireNotContains(t, response.Header.Get("Content-Security-Policy"), "default-src *", "unsafe-eval")

	headResponse, headBody := shareRequest(t, client, http.MethodHead, publicPath, nil, nil)
	if headResponse.StatusCode != http.StatusOK {
		t.Fatalf("public HEAD status = %d, want %d", headResponse.StatusCode, http.StatusOK)
	}
	if headBody != "" {
		t.Errorf("public HEAD body = %q, want empty", headBody)
	}
	if headResponse.ContentLength != int64(len(publicBody)) {
		t.Errorf("HEAD Content-Length = %d, want %d", headResponse.ContentLength, len(publicBody))
	}
	assertSharedResponseHeaders(t, headResponse)
	assertSharedResponseCSP(t, headResponse)

	response, repeatedBody := authGet(t, client, publicPath, nil, nil)
	if response.StatusCode != http.StatusOK || repeatedBody != publicBody {
		t.Fatalf("repeated public bootstrap changed: status %d", response.StatusCode)
	}

	if _, err := database.Exec(`
		UPDATE settings SET theme = '1975' WHERE id = ?;
		UPDATE messages SET content = 'Changed answer' WHERE id = ?;
		DELETE FROM chats WHERE id = ?;
	`, userID, messageID, chatID); err != nil {
		t.Fatalf("change and remove share source: %v", err)
	}

	response, frozenBody := authGet(t, client, publicPath, nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("frozen public bootstrap status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if frozenBody != publicBody {
		t.Fatal("public bootstrap changed after source removal")
	}
	requireContains(t, frozenBody,
		`data-nonce="`+base64.RawURLEncoding.EncodeToString(nonce)+`"`,
		`data-ciphertext="`+base64.RawURLEncoding.EncodeToString(ciphertext)+`"`,
	)
	requireNotContains(t, frozenBody, "Changed answer", "data-theme=")
	if stored = shareStoredResponse(t, database, token); !bytes.Equal(stored.Ciphertext, ciphertext) {
		t.Error("stored ciphertext changed after source removal")
	}
}

func TestMessageSharePublishRejectsInvalidPayloads(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_Invalid_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	chatID := seedShareChat(t, database, userID, "Invalid publish chat")
	messageID := seedShareMessage(t, database, chatID, 0, "assistant", "Invalid publish answer")

	token, encodedSnapshot, _ := prepareShare(t, client, server.URL, cookie, chatID, messageID)
	snapshot := shareDecodedSnapshot(t, encodedSnapshot)
	key := newShareKey(t)
	nonce, ciphertext := encryptShare(t, key, []byte(snapshot))
	valid := sharePublishValues(token, nonce, ciphertext)
	target := sharePublishTarget(server.URL, chatID, messageID)

	cases := []struct {
		name   string
		mutate func(url.Values)
		want   int
	}{
		{name: "unknown key field", mutate: func(values url.Values) { values.Set("key", "not-allowed") }, want: http.StatusBadRequest},
		{name: "duplicate token", mutate: func(values url.Values) {
			values["token"] = append(values["token"], values.Get("token"))
		}, want: http.StatusBadRequest},
		{name: "missing nonce", mutate: func(values url.Values) { values.Del("nonce") }, want: http.StatusBadRequest},
		{name: "unsupported format version", mutate: func(values url.Values) { values.Set("format_version", "2") }, want: http.StatusBadRequest},
		{name: "malformed token", mutate: func(values url.Values) {
			values.Set("token", values.Get("token")+"=")
		}, want: http.StatusBadRequest},
		{name: "malformed nonce", mutate: func(values url.Values) {
			values.Set("nonce", strings.Repeat("A", 15))
		}, want: http.StatusBadRequest},
		{name: "short ciphertext", mutate: func(values url.Values) {
			values.Set("ciphertext", base64.RawURLEncoding.EncodeToString(make([]byte, 15)))
		}, want: http.StatusBadRequest},
		{name: "noncanonical ciphertext", mutate: func(values url.Values) {
			values.Set("ciphertext", values.Get("ciphertext")+"=")
		}, want: http.StatusBadRequest},
		{name: "oversized ciphertext", mutate: func(values url.Values) {
			values.Set("ciphertext", base64.RawURLEncoding.EncodeToString(make([]byte, kritui_db.MaxSharedResponseBytes+1)))
		}, want: http.StatusRequestEntityTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := cloneShareValues(valid)
			testCase.mutate(values)
			response, body := authPostForm(t, client, target, values, cookie, nil)
			if response.StatusCode != testCase.want {
				t.Fatalf("publish status = %d, want %d; body = %q", response.StatusCode, testCase.want, body)
			}
			if count := shareRowCount(t, database); count != 0 {
				t.Fatalf("invalid publish stored %d rows, want 0", count)
			}
		})
	}
}

func TestMessageSharePublishAcceptsMinimumCiphertext(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_Boundary_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	chatID := seedShareChat(t, database, userID, "Boundary chat")
	messageID := seedShareMessage(t, database, chatID, 0, "assistant", "Boundary answer")

	token, _, _ := prepareShare(t, client, server.URL, cookie, chatID, messageID)
	nonce, ciphertext := encryptShare(t, newShareKey(t), nil)
	if len(ciphertext) != 16 {
		t.Fatalf("empty plaintext ciphertext length = %d, want 16", len(ciphertext))
	}
	response, body := authPostForm(t, client, sharePublishTarget(server.URL, chatID, messageID), sharePublishValues(token, nonce, ciphertext), cookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("minimum ciphertext publish status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, body)
	}
	if stored := shareStoredResponse(t, database, token); len(stored.Ciphertext) != 16 {
		t.Errorf("stored ciphertext length = %d, want 16", len(stored.Ciphertext))
	}
}

func TestMessageSharePublishIsIdempotentAndConflicts(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	const username = "Share_Idempotent_Owner"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	userID := shareTestUserID(t, database, username)
	chatID := seedShareChat(t, database, userID, "Idempotent chat")
	messageID := seedShareMessage(t, database, chatID, 0, "assistant", "Idempotent answer")

	token, encodedSnapshot, _ := prepareShare(t, client, server.URL, cookie, chatID, messageID)
	key := newShareKey(t)
	nonce, ciphertext := encryptShare(t, key, []byte(shareDecodedSnapshot(t, encodedSnapshot)))
	values := sharePublishValues(token, nonce, ciphertext)
	target := sharePublishTarget(server.URL, chatID, messageID)

	for attempt := 0; attempt < 2; attempt++ {
		response, body := authPostForm(t, client, target, values, cookie, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("repeat publish %d status = %d, want %d; body = %q", attempt, response.StatusCode, http.StatusOK, body)
		}
	}
	if count := shareRowCount(t, database); count != 1 {
		t.Fatalf("idempotent publish stored %d rows, want 1", count)
	}

	otherNonce, otherCiphertext := encryptShare(t, key, []byte("different snapshot"))
	response, body := authPostForm(t, client, target, sharePublishValues(token, otherNonce, otherCiphertext), cookie, nil)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting publish status = %d, want %d; body = %q", response.StatusCode, http.StatusConflict, body)
	}

	response, body = authPostForm(t, client, target, values, cookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("publish after conflict status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, body)
	}
	stored := shareStoredResponse(t, database, token)
	if !bytes.Equal(stored.Nonce, nonce) || !bytes.Equal(stored.Ciphertext, ciphertext) {
		t.Error("conflicting publish overwrote stored ciphertext")
	}
}

func TestSharedResponseHandlerRejectsUnknownAndMalformedTokens(t *testing.T) {
	database := openTestDatabase(t)
	server := newShareTestServer(t, database)
	client := newAuthTestClient()

	paths := []string{
		"/s/" + strings.Repeat("A", 43), // canonical base64url for 32 zero bytes, absent from storage
		"/s/" + strings.Repeat("a", 42) + "=",
		"/s/" + strings.Repeat("a", 44),
		"/s/short",
		"/s/not-a-token",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response, _ := shareRequest(t, client, method, server.URL+path, nil, nil)
				if response.StatusCode != http.StatusNotFound {
					t.Errorf("%s %s status = %d, want %d", method, path, response.StatusCode, http.StatusNotFound)
				}
			}
		})
	}
}

func shareRequest(t *testing.T, client *http.Client, method, target string, cookie *http.Cookie, headers map[string]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return authDo(t, client, request)
}

func assertSharedResponseColumnsAreCiphertextOnly(t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.Query(`SELECT name FROM pragma_table_info('shared_responses')`)
	if err != nil {
		t.Fatalf("inspect shared response columns: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan shared response column: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate shared response columns: %v", err)
	}
	for _, name := range []string{"format_version", "nonce", "ciphertext"} {
		if !columns[name] {
			t.Errorf("shared response table lacks %q", name)
		}
	}
	for _, name := range []string{"html", "theme"} {
		if columns[name] {
			t.Errorf("shared response table still stores %q", name)
		}
	}
	for name := range columns {
		if strings.Contains(strings.ToLower(name), "key") {
			t.Errorf("shared response table has key material column %q", name)
		}
	}
}

func assertSharedResponseCSP(t *testing.T, response *http.Response) {
	t.Helper()
	csp := response.Header.Get("Content-Security-Policy")
	if csp != templates.SharedResponseCSP() {
		t.Errorf("Content-Security-Policy = %q, want templates.SharedResponseCSP()", csp)
	}
	if !strings.Contains(csp, "script-src 'sha256-") {
		t.Errorf("Content-Security-Policy = %q, want a hash-authorized inline script", csp)
	}
}

func assertSharedResponseHeaders(t *testing.T, response *http.Response) {
	t.Helper()
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	if robots := response.Header.Get("X-Robots-Tag"); !strings.Contains(robots, "noindex") || !strings.Contains(robots, "nofollow") {
		t.Errorf("X-Robots-Tag = %q, want noindex and nofollow", robots)
	}
	if response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", response.Header.Get("Referrer-Policy"))
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", response.Header.Get("X-Content-Type-Options"))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", contentType)
	}
}
