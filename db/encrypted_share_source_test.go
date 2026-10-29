package kritui_db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"seesharpsi/kritui/llm"
)

func encryptedShareSourceContext(t *testing.T) context.Context {
	t.Helper()
	return WithStorageCipher(WithUser(context.Background(), LegacyOwnerID), newTestStorageCipher(t))
}

func openEncryptedShareSourceDatabase(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedShareSourceContext(t)
	if err := UpsertChat(ctx, database, 1, "Encrypted share title", nil, nil); err != nil {
		t.Fatalf("UpsertChat() error: %v", err)
	}
	return database, ctx
}

func TestEncryptedShareSourceDecryptsNearestActiveUser(t *testing.T) {
	database, ctx := openEncryptedShareSourceDatabase(t)
	if _, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "user", Content: "older question"}); err != nil {
		t.Fatalf("InsertMessage(older) error: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 1, llm.Message{Role: "user", Content: "target question"}); err != nil {
		t.Fatalf("InsertMessage(target) error: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 2, llm.Message{
		Role:    "assistant",
		Content: "calling tool",
		ToolCalls: []llm.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`},
		}},
	}); err != nil {
		t.Fatalf("InsertMessage(tool call) error: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 3, llm.Message{Role: "tool", Content: "tool result", ToolCallID: "call-1"}); err != nil {
		t.Fatalf("InsertMessage(tool result) error: %v", err)
	}
	hiddenID, err := InsertMessage(ctx, database, 1, 4, llm.Message{Role: "user", Content: "hidden closer question"})
	if err != nil {
		t.Fatalf("InsertMessage(hidden user) error: %v", err)
	}
	finalID, err := InsertMessage(ctx, database, 1, 5, llm.Message{Role: "assistant", Content: "<p>final answer</p>"})
	if err != nil {
		t.Fatalf("InsertMessage(final answer) error: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 6, llm.Message{Role: "user", Content: "later question"}); err != nil {
		t.Fatalf("InsertMessage(later) error: %v", err)
	}
	if _, err := database.Exec(`UPDATE messages SET undo_sequence = 1 WHERE id = ?`, hiddenID); err != nil {
		t.Fatalf("hide closer user message: %v", err)
	}

	var storedTitle, storedContent string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&storedTitle); err != nil {
		t.Fatalf("read raw title: %v", err)
	}
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, finalID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw final content: %v", err)
	}
	requireEncryptedText(t, storedTitle, "Encrypted share title")
	requireEncryptedText(t, storedContent, "<p>final answer</p>")

	source, err := GetShareSource(ctx, database, 1, finalID)
	if err != nil {
		t.Fatalf("GetShareSource() error: %v", err)
	}
	if source.Title != "Encrypted share title" || source.UserContent != "target question" || source.Content != "<p>final answer</p>" {
		t.Fatalf("GetShareSource() = %#v, want decrypted nearest active user and final answer", source)
	}
}

func TestEncryptedShareSourceHandlesMissingAndEmptyUserContent(t *testing.T) {
	database, ctx := openEncryptedShareSourceDatabase(t)
	if err := UpsertChat(ctx, database, 2, "No prior title", nil, nil); err != nil {
		t.Fatalf("UpsertChat(no prior) error: %v", err)
	}
	noPriorID, err := InsertMessage(ctx, database, 2, 0, llm.Message{Role: "assistant", Content: "no prior answer"})
	if err != nil {
		t.Fatalf("InsertMessage(no prior answer) error: %v", err)
	}
	source, err := GetShareSource(ctx, database, 2, noPriorID)
	if err != nil {
		t.Fatalf("GetShareSource(no prior) error: %v", err)
	}
	if source.Title != "No prior title" || source.UserContent != "" || source.Content != "no prior answer" {
		t.Fatalf("GetShareSource(no prior) = %#v, want empty user content", source)
	}

	if err := UpsertChat(ctx, database, 3, "Empty user title", nil, nil); err != nil {
		t.Fatalf("UpsertChat(empty user) error: %v", err)
	}
	emptyUserID, err := InsertMessage(ctx, database, 3, 0, llm.Message{Role: "user", Content: ""})
	if err != nil {
		t.Fatalf("InsertMessage(empty user) error: %v", err)
	}
	emptyAnswerID, err := InsertMessage(ctx, database, 3, 1, llm.Message{Role: "assistant", Content: "empty user answer"})
	if err != nil {
		t.Fatalf("InsertMessage(empty answer) error: %v", err)
	}
	source, err = GetShareSource(ctx, database, 3, emptyAnswerID)
	if err != nil {
		t.Fatalf("GetShareSource(empty user) error: %v", err)
	}
	if source.UserContent != "" || source.Content != "empty user answer" {
		t.Fatalf("GetShareSource(empty user) = %#v, want authenticated empty user content", source)
	}

	if _, err := database.Exec(`UPDATE messages SET content = '' WHERE id = ?`, emptyUserID); err != nil {
		t.Fatalf("tamper empty user content: %v", err)
	}
	if _, err := GetShareSource(ctx, database, 3, emptyAnswerID); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("plaintext empty user content error = %v, want ErrStorageDecryption", err)
	}
}

func TestEncryptedShareSourceRejectsTamperedTitleAndContent(t *testing.T) {
	t.Run("title", func(t *testing.T) {
		database, ctx := openEncryptedShareSourceDatabase(t)
		finalID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "assistant", Content: "answer"})
		if err != nil {
			t.Fatalf("InsertMessage() error: %v", err)
		}
		if _, err := database.Exec(`UPDATE chats SET title = 'plain title' WHERE id = 1`); err != nil {
			t.Fatalf("tamper title: %v", err)
		}
		if _, err := GetShareSource(ctx, database, 1, finalID); !errors.Is(err, ErrStorageDecryption) {
			t.Fatalf("tampered title error = %v, want ErrStorageDecryption", err)
		}
	})

	t.Run("assistant content", func(t *testing.T) {
		database, ctx := openEncryptedShareSourceDatabase(t)
		finalID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "assistant", Content: "answer"})
		if err != nil {
			t.Fatalf("InsertMessage() error: %v", err)
		}
		if _, err := database.Exec(`UPDATE messages SET content = 'plain answer' WHERE id = ?`, finalID); err != nil {
			t.Fatalf("tamper assistant content: %v", err)
		}
		if _, err := GetShareSource(ctx, database, 1, finalID); !errors.Is(err, ErrStorageDecryption) {
			t.Fatalf("tampered assistant content error = %v, want ErrStorageDecryption", err)
		}
	})

	t.Run("preceding user content", func(t *testing.T) {
		database, ctx := openEncryptedShareSourceDatabase(t)
		userID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "user", Content: "question"})
		if err != nil {
			t.Fatalf("InsertMessage(user) error: %v", err)
		}
		finalID, err := InsertMessage(ctx, database, 1, 1, llm.Message{Role: "assistant", Content: "answer"})
		if err != nil {
			t.Fatalf("InsertMessage(assistant) error: %v", err)
		}
		if _, err := database.Exec(`UPDATE messages SET content = 'plain question' WHERE id = ?`, userID); err != nil {
			t.Fatalf("tamper user content: %v", err)
		}
		if _, err := GetShareSource(ctx, database, 1, finalID); !errors.Is(err, ErrStorageDecryption) {
			t.Fatalf("tampered user content error = %v, want ErrStorageDecryption", err)
		}
	})
}

func TestEncryptedShareSourceLeavesPublicShareStorageUnchanged(t *testing.T) {
	database, ctx := openEncryptedShareSourceDatabase(t)
	finalID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "assistant", Content: "answer"})
	if err != nil {
		t.Fatalf("InsertMessage() error: %v", err)
	}
	nonce, ciphertext := shareTestEncryptedPayload()
	share := SharedResponse{
		TokenHash:     shareTestToken,
		ChatID:        1,
		MessageID:     finalID,
		FormatVersion: SharedResponseFormatVersion,
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	}
	if err := CreateSharedResponse(ctx, database, share); err != nil {
		t.Fatalf("CreateSharedResponse() error: %v", err)
	}
	stored, err := GetSharedResponse(ctx, database, shareTestToken)
	if err != nil {
		t.Fatalf("GetSharedResponse() error: %v", err)
	}
	if !bytes.Equal(stored.Nonce, nonce) || !bytes.Equal(stored.Ciphertext, ciphertext) {
		t.Fatalf("stored public share changed: nonce %x ciphertext %x", stored.Nonce, stored.Ciphertext)
	}
}
