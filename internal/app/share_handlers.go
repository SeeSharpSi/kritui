package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/themes"
)

const (
	// maxShareBodyBytes bounds editable Markdown plus share options.
	maxShareBodyBytes     int64 = (3 << 20) + (32 << 10)
	maxShareMarkdownBytes       = 1 << 20
	// maxSharePublishBodyBytes bounds the encoded ciphertext upload. The
	// decoded ciphertext itself is capped by kritui_db.MaxSharedResponseBytes.
	maxSharePublishBodyBytes int64 = 24 << 20
	// sharedTokenBytes is the random entropy behind one public share path.
	sharedTokenBytes = 32
	// sharedTokenLength is the canonical unpadded base64url length.
	sharedTokenLength = 43
	// sharedNonceBytes is the AES-GCM nonce length chosen by the sender.
	sharedNonceBytes = 12
	// sharedNonceLength is the canonical unpadded base64url nonce length.
	sharedNonceLength = 16
)

// messageShareHandler prepares one standalone plaintext snapshot for an active
// final assistant message owned by the signed-in account. The snapshot and the
// new token are returned to the browser only; nothing is persisted here.
func messageShareHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if r.Header.Get("HX-Request") != "true" {
			http.Error(w, "Sharing requires JavaScript.", http.StatusBadRequest)
			return
		}

		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			http.Error(w, "A valid chat is required.", http.StatusBadRequest)
			return
		}
		messageID, ok := positiveID(r.PathValue("message"))
		if !ok {
			http.Error(w, "A valid message is required.", http.StatusBadRequest)
			return
		}
		defer removeMultipartForm(r)
		if err := parseLimitedForm(w, r, maxShareBodyBytes); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "Request body is too large.", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Invalid share form.", http.StatusBadRequest)
			return
		}

		source, err := kritui_db.GetShareSource(r.Context(), database, chatID, messageID)
		switch {
		case errors.Is(err, kritui_db.ErrMessageNotFound), errors.Is(err, kritui_db.ErrChatNotFound), errors.Is(err, sql.ErrNoRows):
			http.Error(w, "Message not found.", http.StatusNotFound)
			return
		case err != nil:
			log.Printf("get share source: %v", err)
			http.Error(w, "Failed to share message.", http.StatusInternalServerError)
			return
		}

		theme, err := loadStoredTheme(r.Context(), database)
		if err != nil {
			log.Printf("load share theme: %v", err)
			http.Error(w, "Failed to share message.", http.StatusInternalServerError)
			return
		}

		title, content, mode, theme, status := parseShareOptions(r.PostForm, source, theme)
		if status != 0 {
			http.Error(w, http.StatusText(status)+".", status)
			return
		}
		if mode == "whole" {
			conversation, err := kritui_db.GetShareConversation(r.Context(), database, chatID)
			switch {
			case errors.Is(err, kritui_db.ErrChatNotFound):
				http.Error(w, "Chat not found.", http.StatusNotFound)
				return
			case err != nil:
				log.Printf("get share conversation: %v", err)
				http.Error(w, "Failed to share conversation.", http.StatusInternalServerError)
				return
			}
			var transcript strings.Builder
			for _, message := range conversation.Messages {
				if strings.TrimSpace(message.Content) == "" {
					continue
				}
				if transcript.Len()+len(message.Content)+16 > kritui_db.MaxSharedResponseBytes-16 {
					http.Error(w, "Shared response is too large.", http.StatusRequestEntityTooLarge)
					return
				}
				if transcript.Len() > 0 {
					transcript.WriteString("\n\n")
				}
				if message.Role == "user" {
					transcript.WriteString("## User\n\n")
				} else {
					transcript.WriteString("## Assistant\n\n")
				}
				transcript.WriteString(message.Content)
			}
			content = transcript.String()
		}
		createdAt := time.Now().UTC()
		var snapshot bytes.Buffer
		userContent := source.UserContent
		if mode == "whole" {
			userContent = ""
		}
		if err := templates.SharedResponsePage(title, userContent, content, createdAt, theme).Render(r.Context(), &snapshot); err != nil {
			log.Printf("render shared response: %v", err)
			http.Error(w, "Failed to share message.", http.StatusInternalServerError)
			return
		}
		if snapshot.Len() > kritui_db.MaxSharedResponseBytes-16 {
			http.Error(w, "Shared response is too large.", http.StatusRequestEntityTooLarge)
			return
		}

		token, err := newSharedToken()
		if err != nil {
			log.Printf("generate share token: %v", err)
			http.Error(w, "Failed to share message.", http.StatusInternalServerError)
			return
		}

		encodedSnapshot := base64.RawURLEncoding.EncodeToString(snapshot.Bytes())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.PreparedMessageShare(token, encodedSnapshot).Render(r.Context(), w); err != nil {
			log.Printf("render prepared message share: %v", err)
		}
	}
}

func parseShareOptions(values map[string][]string, source kritui_db.ShareSource, currentTheme themes.Theme) (title, content, mode string, theme themes.Theme, status int) {
	theme = currentTheme
	title = strings.TrimSpace(source.Title)
	if title == "" {
		title = "Shared response"
	}
	content = source.Content
	mode = "recent"
	allowed := map[string]bool{"title": true, "markdown": true, "mode": true, "theme": true}
	if len(values) == 0 {
		return title, content, mode, theme, 0
	}
	for key, entries := range values {
		if !allowed[key] || len(entries) != 1 {
			return "", "", "", themes.Theme{}, http.StatusBadRequest
		}
	}
	for _, name := range []string{"title", "markdown", "mode", "theme"} {
		if len(values[name]) != 1 {
			return "", "", "", themes.Theme{}, http.StatusBadRequest
		}
	}
	title = strings.TrimSpace(values["title"][0])
	content = values["markdown"][0]
	mode = values["mode"][0]
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxChatTitleRunes ||
		!utf8.ValidString(content) || len(content) > maxShareMarkdownBytes || (mode != "recent" && mode != "whole") {
		return "", "", "", themes.Theme{}, http.StatusBadRequest
	}
	selectedTheme, err := themes.ByID(values["theme"][0])
	if err != nil {
		return "", "", "", themes.Theme{}, http.StatusBadRequest
	}
	return title, content, mode, selectedTheme, 0
}

// messageSharePublishHandler stores only the ciphertext prepared and encrypted
// by the browser. The encryption key never reaches this handler or the database.
func messageSharePublishHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			http.Error(w, "A valid chat is required.", http.StatusBadRequest)
			return
		}
		messageID, ok := positiveID(r.PathValue("message"))
		if !ok {
			http.Error(w, "A valid message is required.", http.StatusBadRequest)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxSharePublishBodyBytes)
		if err := r.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "Request body is too large.", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Invalid share payload.", http.StatusBadRequest)
			return
		}

		token, formatVersion, nonce, ciphertext, status := parsePublishedShare(r.PostForm)
		if status != 0 {
			http.Error(w, http.StatusText(status)+".", status)
			return
		}

		err := kritui_db.CreateSharedResponse(r.Context(), database, kritui_db.SharedResponse{
			TokenHash:     hashSharedToken(token),
			ChatID:        chatID,
			MessageID:     messageID,
			FormatVersion: formatVersion,
			Nonce:         nonce,
			Ciphertext:    ciphertext,
		})
		switch {
		case errors.Is(err, kritui_db.ErrShareConflict):
			http.Error(w, "A different shared response already uses this token.", http.StatusConflict)
			return
		case errors.Is(err, kritui_db.ErrMessageNotFound), errors.Is(err, kritui_db.ErrChatNotFound), errors.Is(err, sql.ErrNoRows):
			http.Error(w, "Message not found.", http.StatusNotFound)
			return
		case err != nil:
			log.Printf("create shared response: %v", err)
			http.Error(w, "Failed to share message.", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.MessageShareActions(strconv.FormatInt(chatID, 10), messageID, "", "/s/"+token).Render(r.Context(), w); err != nil {
			log.Printf("render message share actions: %v", err)
		}
	}
}

// parsePublishedShare validates the exact publish form. A non-zero HTTP status
// reports the first problem without echoing client-controlled values.
func parsePublishedShare(values map[string][]string) (string, int, []byte, []byte, int) {
	allowed := map[string]bool{
		"token":          true,
		"format_version": true,
		"nonce":          true,
		"ciphertext":     true,
	}
	if len(values) != len(allowed) {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	for name, entries := range values {
		if !allowed[name] || len(entries) != 1 {
			return "", 0, nil, nil, http.StatusBadRequest
		}
	}

	token, ok := canonicalSharedToken(values["token"][0])
	if !ok {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	if values["format_version"][0] != strconv.Itoa(kritui_db.SharedResponseFormatVersion) {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	nonce, ok := decodeCanonicalBase64URL(values["nonce"][0], sharedNonceLength, sharedNonceBytes)
	if !ok {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	ciphertextValue := values["ciphertext"][0]
	if ciphertextValue == "" {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(ciphertextValue)
	if err != nil || base64.RawURLEncoding.EncodeToString(ciphertext) != ciphertextValue {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	if len(ciphertext) < 16 {
		return "", 0, nil, nil, http.StatusBadRequest
	}
	if len(ciphertext) > kritui_db.MaxSharedResponseBytes {
		return "", 0, nil, nil, http.StatusRequestEntityTooLarge
	}
	return token, kritui_db.SharedResponseFormatVersion, nonce, ciphertext, 0
}

func decodeCanonicalBase64URL(value string, length, decodedLength int) ([]byte, bool) {
	if len(value) != length {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != decodedLength {
		return nil, false
	}
	if base64.RawURLEncoding.EncodeToString(raw) != value {
		return nil, false
	}
	return raw, true
}

// sharedResponseHandler serves the public encrypted bootstrap by token. It
// reads only the stored ciphertext record; the plaintext snapshot and its
// account, source, and theme are never consulted.
func sharedResponseHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Robots-Tag", "noindex,nofollow")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", templates.SharedResponseCSP())

		token, ok := canonicalSharedToken(r.PathValue("token"))
		if !ok {
			http.Error(w, "Shared response not found.", http.StatusNotFound)
			return
		}

		share, err := kritui_db.GetSharedResponse(r.Context(), database, hashSharedToken(token))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, "Shared response not found.", http.StatusNotFound)
			return
		case err != nil:
			log.Printf("get shared response: %v", err)
			http.Error(w, "Failed to load shared response.", http.StatusInternalServerError)
			return
		}

		var bootstrap bytes.Buffer
		if err := templates.EncryptedSharedResponsePage(
			share.FormatVersion,
			base64.RawURLEncoding.EncodeToString(share.Nonce),
			base64.RawURLEncoding.EncodeToString(share.Ciphertext),
		).Render(r.Context(), &bootstrap); err != nil {
			log.Printf("render encrypted shared response: %v", err)
			http.Error(w, "Failed to load shared response.", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(bootstrap.Len()))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(bootstrap.Bytes())
	}
}

func newSharedToken() (string, error) {
	raw := make([]byte, sharedTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func canonicalSharedToken(token string) (string, bool) {
	if len(token) != sharedTokenLength {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != sharedTokenBytes {
		return "", false
	}
	if base64.RawURLEncoding.EncodeToString(raw) != token {
		return "", false
	}
	return token, true
}

func hashSharedToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
