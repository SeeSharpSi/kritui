package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
)

const (
	titleGenerationTimeout      = 20 * time.Second
	titleGenerationSystemPrompt = `You are a model that generates chat titles based on the user's first chat message.
You will be given the user's message, and must generate a title with these rules:
1. Concise; 10 words max
2. No trailing period
3. No markdown; just plain text
4. Clearly shows what the message was requesting and what the chat will be about`
)

// chatTitleHandler generates a title in its own HTTP request, independently of
// chat completion. Accepted work uses a detached context so navigating away
// from the page does not cancel a provider request that may still be useful.
func chatTitleHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		if err := parseLimitedForm(w, r, maxRetryBodyBytes); err != nil {
			renderMessageError(w, r, http.StatusBadRequest, "Invalid title request.")
			return
		}
		owned, err := requireOwnedChat(r.Context(), database, chatID)
		if err != nil {
			log.Printf("check title chat ownership: %v", err)
			renderMessageError(w, r, http.StatusInternalServerError, "Failed to generate title.")
			return
		}
		if !owned {
			renderMessageError(w, r, http.StatusNotFound, "Chat not found.")
			return
		}

		userID := kritui_db.UserID(r.Context())
		ctx := kritui_db.WithUser(context.WithoutCancel(r.Context()), userID)
		started := time.Now()
		updated, err := generateChatTitle(ctx, database, chatID)
		if err != nil {
			log.Printf("chat %d: generate title after %s: %v", chatID, time.Since(started).Round(time.Millisecond), err)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ChatTitleComplete().Render(r.Context(), w); err != nil {
			log.Printf("chat %d: render title completion: %v", chatID, err)
			return
		}
		if err != nil || !updated {
			return
		}
		refresh, err := renderHistoryRefresh(ctx, database, strconv.FormatInt(chatID, 10))
		if err != nil {
			log.Printf("chat %d: render title history refresh: %v", chatID, err)
			return
		}
		if _, err := io.WriteString(w, refresh); err != nil {
			log.Printf("chat %d: write title history refresh: %v", chatID, err)
		}
	}
}

// generateChatTitle makes one best-effort title request for a chat whose title
// still matches the fallback derived from its first active user message. The
// boolean is true only when the stored title actually changed.
func generateChatTitle(ctx context.Context, database *sql.DB, chatID int64) (bool, error) {
	settings, err := kritui_db.GetTitleGenerationSettings(ctx, database)
	if err != nil {
		return false, fmt.Errorf("load title generation settings: %w", err)
	}
	model := strings.TrimSpace(settings.Model)
	if !settings.Enabled || model == "" {
		return false, nil
	}

	messages, err := kritui_db.GetMessages(ctx, database, chatID)
	if err != nil {
		return false, fmt.Errorf("load title generation history: %w", err)
	}
	first, ok := firstActiveUserMessage(messages)
	if !ok {
		return false, nil
	}
	expectedTitle := normalizeChatTitle(first.Content)
	currentTitle, err := kritui_db.GetChatTitle(ctx, database, chatID)
	if err != nil {
		return false, fmt.Errorf("load current chat title: %w", err)
	}
	if currentTitle != expectedTitle {
		return false, nil
	}

	config, err := effectiveLLMConfig(ctx, database)
	if err != nil {
		return false, fmt.Errorf("load title model connection: %w", err)
	}
	preferredEndpoint, err := kritui_db.GetModelEndpointType(ctx, database, model)
	if err != nil {
		return false, fmt.Errorf("load title model endpoint type: %w", err)
	}
	sessionID, err := newTitleSessionID()
	if err != nil {
		return false, err
	}
	client, err := llm.New(config.APIKey, model, config.Endpoint, llm.ClientOptions{
		SessionID:         sessionID,
		PreferredEndpoint: preferredEndpoint,
		EndpointSelected: func(endpointType llm.EndpointType) {
			if err := kritui_db.SetModelEndpointType(ctx, database, model, endpointType); err != nil {
				log.Printf("store title model endpoint type: %v", err)
			}
		},
	})
	if err != nil {
		return false, fmt.Errorf("configure title model: %w", err)
	}

	title, err := generateTitleWithClient(ctx, client, first)
	if err != nil {
		return false, err
	}
	if title == "" {
		return false, nil
	}
	updated, err := kritui_db.UpdateChatTitleIfUnchanged(ctx, database, chatID, expectedTitle, title)
	if err != nil {
		return false, fmt.Errorf("store generated chat title: %w", err)
	}
	return updated, nil
}

func generateTitleWithClient(ctx context.Context, client *llm.Client, message llm.Message) (string, error) {
	requestContext, cancel := context.WithTimeout(ctx, titleGenerationTimeout)
	defer cancel()

	title, err := client.CompleteText(requestContext, titleGenerationSystemPrompt, message.Content)
	if err != nil {
		return "", fmt.Errorf("generate title: %w", err)
	}
	return cleanGeneratedChatTitle(title), nil
}

// newTitleSessionID gives title requests an independent provider operation
// identity rather than reusing the chat completion session.
func newTitleSessionID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate title session ID: %w", err)
	}
	return "title-" + hex.EncodeToString(token[:]), nil
}

func firstActiveUserMessage(messages []llm.Message) (llm.Message, bool) {
	for _, message := range messages {
		if message.Role == "user" {
			return message, true
		}
	}
	return llm.Message{}, false
}

// isInitialUserTurn reports whether accepted history contains exactly the first
// user message, so only a freshly started chat requests a generated title.
func isInitialUserTurn(messages []llm.Message) bool {
	return len(messages) == 1 && messages[0].Role == "user"
}

func cleanGeneratedChatTitle(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "title:") {
		value = strings.TrimSpace(value[len("title:"):])
	}
	value = strings.Trim(value, " \t\r\n\"'`*_")
	return normalizeChatTitle(value)
}
