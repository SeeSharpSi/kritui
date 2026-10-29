package kritui_db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func encryptedMessagesTestContext(t *testing.T) context.Context {
	t.Helper()
	return WithStorageCipher(WithUser(context.Background(), LegacyOwnerID), newTestStorageCipher(t))
}

func requireEncryptedText(t *testing.T, stored, plaintext string) {
	t.Helper()
	if !strings.HasPrefix(stored, storageTextPrefix) {
		t.Fatalf("stored text %q lacks text envelope prefix", stored)
	}
	if plaintext != "" && strings.Contains(stored, plaintext) {
		t.Fatalf("stored text %q exposes plaintext %q", stored, plaintext)
	}
}

func requireEncryptedBytes(t *testing.T, stored, plaintext []byte) {
	t.Helper()
	if bytes.Equal(stored, plaintext) || bytes.Contains(stored, plaintext) {
		t.Fatalf("stored blob %x exposes plaintext %x", stored, plaintext)
	}
	if len(stored) <= len(plaintext) {
		t.Fatalf("stored blob length = %d, want larger than plaintext length %d", len(stored), len(plaintext))
	}
}

func TestEncryptedChatAndMessageFieldsRoundTrip(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedMessagesTestContext(t)

	const title = "Secret chat title"
	if err := UpsertChat(ctx, database, 1, title, []string{"lookup"}, []string{"research"}); err != nil {
		t.Fatalf("UpsertChat() error: %v", err)
	}
	var storedTitle string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&storedTitle); err != nil {
		t.Fatalf("read raw title: %v", err)
	}
	requireEncryptedText(t, storedTitle, title)

	metadata, err := llm.NewResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"reasoning-secret"}`),
	})
	if err != nil {
		t.Fatalf("NewResponsesProviderMetadata() error: %v", err)
	}
	attachmentData := []byte("%PDF-1.7\nsecret attachment\n")
	userMessageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{
		Role:              "user",
		Content:           "secret question",
		PromptAppendTexts: []string{"secret append"},
		Attachments: []llm.UserAttachment{{
			Filename:  "secret-report.pdf",
			MediaType: "application/pdf",
			Data:      attachmentData,
		}},
	})
	if err != nil {
		t.Fatalf("InsertMessage(user) error: %v", err)
	}
	assistantMessageID, err := InsertMessage(ctx, database, 1, 1, llm.Message{
		Role:    "assistant",
		Content: "secret answer",
		ToolCalls: []llm.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "lookup", Arguments: `{"query":"secret argument"}`},
		}},
		ProviderMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("InsertMessage(assistant) error: %v", err)
	}

	var storedContent string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, userMessageID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw user content: %v", err)
	}
	requireEncryptedText(t, storedContent, "secret question")
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, assistantMessageID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw assistant content: %v", err)
	}
	requireEncryptedText(t, storedContent, "secret answer")

	var storedArguments string
	if err := database.QueryRow(`SELECT arguments FROM message_tool_calls WHERE message_id = ?`, assistantMessageID).Scan(&storedArguments); err != nil {
		t.Fatalf("read raw tool arguments: %v", err)
	}
	requireEncryptedText(t, storedArguments, "secret argument")

	var storedAppend string
	if err := database.QueryRow(`SELECT text FROM message_prompt_appends WHERE message_id = ?`, userMessageID).Scan(&storedAppend); err != nil {
		t.Fatalf("read raw prompt append: %v", err)
	}
	requireEncryptedText(t, storedAppend, "secret append")

	var (
		storedFilename string
		storedData     []byte
	)
	if err := database.QueryRow(`
		SELECT filename, data FROM message_user_images WHERE message_id = ?
	`, userMessageID).Scan(&storedFilename, &storedData); err != nil {
		t.Fatalf("read raw attachment: %v", err)
	}
	requireEncryptedText(t, storedFilename, "secret-report.pdf")
	requireEncryptedBytes(t, storedData, attachmentData)

	var storedPayload string
	if err := database.QueryRow(`SELECT payload FROM message_provider_outputs WHERE message_id = ?`, assistantMessageID).Scan(&storedPayload); err != nil {
		t.Fatalf("read raw provider payload: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(storedPayload), &envelope); err != nil {
		t.Fatalf("decode provider envelope: %v", err)
	}
	rawEnvelope, ok := envelope["encrypted"]
	if !ok || len(envelope) != 1 {
		t.Fatalf("provider payload = %s, want one encrypted field", storedPayload)
	}
	var envelopeText string
	if err := json.Unmarshal(rawEnvelope, &envelopeText); err != nil {
		t.Fatalf("decode provider envelope text: %v", err)
	}
	requireEncryptedText(t, envelopeText, "reasoning-secret")

	gotTitle, err := GetChatTitle(ctx, database, 1)
	if err != nil || gotTitle != title {
		t.Fatalf("GetChatTitle() = %q, %v; want %q", gotTitle, err, title)
	}
	chats, err := GetChatsPage(ctx, database, "", 0, false, 10)
	if err != nil {
		t.Fatalf("GetChatsPage() error: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != title {
		t.Fatalf("GetChatsPage() titles = %#v, want %q", chats, title)
	}

	messages, err := GetMessages(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessages() error: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	if messages[0].Content != "secret question" || len(messages[0].PromptAppendTexts) != 1 || messages[0].PromptAppendTexts[0] != "secret append" {
		t.Fatalf("user message = %#v", messages[0])
	}
	if len(messages[0].Attachments) != 1 || messages[0].Attachments[0].Filename != "secret-report.pdf" || !bytes.Equal(messages[0].Attachments[0].Data, attachmentData) {
		t.Fatalf("user attachments = %#v", messages[0].Attachments)
	}
	if messages[1].Content != "secret answer" || len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].Function.Arguments != `{"query":"secret argument"}` {
		t.Fatalf("assistant message = %#v", messages[1])
	}
	outputs := messages[1].ProviderMetadata.ResponsesOutput()
	if len(outputs) != 1 || !strings.Contains(string(outputs[0]), "reasoning-secret") {
		t.Fatalf("provider outputs = %s", outputs)
	}

	attachment, err := GetMessageAttachment(ctx, database, 1, userMessageID, 0)
	if err != nil {
		t.Fatalf("GetMessageAttachment() error: %v", err)
	}
	if attachment.Filename != "secret-report.pdf" || !bytes.Equal(attachment.Data, attachmentData) {
		t.Fatalf("attachment = %#v", attachment)
	}
}

func TestEncryptedChatTitleCompareAndSwapAndRename(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedMessagesTestContext(t)
	if err := UpsertChat(ctx, database, 1, "First fallback", nil, nil); err != nil {
		t.Fatalf("UpsertChat() error: %v", err)
	}

	updated, err := UpdateChatTitleIfUnchanged(ctx, database, 1, "First fallback", "Generated title")
	if err != nil || !updated {
		t.Fatalf("conditional update = %v, %v; want true", updated, err)
	}
	var generatedRaw string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&generatedRaw); err != nil {
		t.Fatalf("read generated raw title: %v", err)
	}
	requireEncryptedText(t, generatedRaw, "Generated title")

	if err := RenameChat(ctx, database, 1, "Manual rename"); err != nil {
		t.Fatalf("RenameChat() error: %v", err)
	}
	var manualRaw string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&manualRaw); err != nil {
		t.Fatalf("read manual raw title: %v", err)
	}
	requireEncryptedText(t, manualRaw, "Manual rename")
	if manualRaw == generatedRaw {
		t.Fatal("manual rename reused the previous ciphertext")
	}

	updated, err = UpdateChatTitleIfUnchanged(ctx, database, 1, "Generated title", "Late title")
	if err != nil {
		t.Fatalf("late conditional update error: %v", err)
	}
	if updated {
		t.Fatal("conditional update overwrote a manual rename")
	}
	var lateRaw string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 1`).Scan(&lateRaw); err != nil {
		t.Fatalf("read late raw title: %v", err)
	}
	if lateRaw != manualRaw {
		t.Fatal("failed conditional update rewrote the stored ciphertext")
	}
	title, err := GetChatTitle(ctx, database, 1)
	if err != nil || title != "Manual rename" {
		t.Fatalf("GetChatTitle() = %q, %v; want Manual rename", title, err)
	}

	_, otherCtx := createIsolationUser(t, database, "EncryptedTitleOther")
	otherCtx = WithStorageCipher(otherCtx, newTestStorageCipher(t))
	if err := RenameChat(otherCtx, database, 1, "Stolen title"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("foreign RenameChat() error = %v, want ErrChatNotFound", err)
	}
	if err := RenameChat(ctx, database, 999, "Missing title"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("missing RenameChat() error = %v, want ErrChatNotFound", err)
	}
}

func TestEncryptedMessageEditAndUndoReturnPlaintext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedMessagesTestContext(t)
	if err := UpsertChat(ctx, database, 1, "Chat", nil, nil); err != nil {
		t.Fatalf("UpsertChat() error: %v", err)
	}
	userMessageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "user", Content: "original secret"})
	if err != nil {
		t.Fatalf("InsertMessage(user) error: %v", err)
	}
	if _, err := InsertMessage(ctx, database, 1, 1, llm.Message{Role: "assistant", Content: "secret answer"}); err != nil {
		t.Fatalf("InsertMessage(assistant) error: %v", err)
	}

	if err := ReplaceUserMessage(ctx, database, 1, userMessageID, llm.Message{
		Role:              "user",
		Content:           "edited secret",
		PromptAppendTexts: []string{"edited append"},
	}); err != nil {
		t.Fatalf("ReplaceUserMessage() error: %v", err)
	}
	var storedContent, storedAppend string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, userMessageID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw edited content: %v", err)
	}
	requireEncryptedText(t, storedContent, "edited secret")
	if err := database.QueryRow(`SELECT text FROM message_prompt_appends WHERE message_id = ?`, userMessageID).Scan(&storedAppend); err != nil {
		t.Fatalf("read raw edited append: %v", err)
	}
	requireEncryptedText(t, storedAppend, "edited append")

	messages, err := GetMessages(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessages() error: %v", err)
	}
	if len(messages) != 1 || messages[0].Content != "edited secret" || len(messages[0].PromptAppendTexts) != 1 || messages[0].PromptAppendTexts[0] != "edited append" {
		t.Fatalf("edited messages = %#v", messages)
	}

	undone, err := UndoLatestTurn(ctx, database, 1)
	if err != nil {
		t.Fatalf("UndoLatestTurn() error: %v", err)
	}
	if undone.Message.Content != "edited secret" {
		t.Fatalf("undone content = %q, want edited secret", undone.Message.Content)
	}
	if len(undone.Messages) != 0 {
		t.Fatalf("active messages after undo = %#v, want none", undone.Messages)
	}
}

func TestEncryptedStorageRejectsTamperedAndPlaintextValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedMessagesTestContext(t)
	if err := UpsertChat(ctx, database, 1, "Secret title", nil, nil); err != nil {
		t.Fatalf("UpsertChat() error: %v", err)
	}
	messageID, err := InsertMessage(ctx, database, 1, 0, llm.Message{Role: "user", Content: "secret question"})
	if err != nil {
		t.Fatalf("InsertMessage() error: %v", err)
	}
	metadata, err := llm.NewResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"reasoning-secret"}`),
	})
	if err != nil {
		t.Fatalf("NewResponsesProviderMetadata() error: %v", err)
	}
	assistantID, err := InsertMessage(ctx, database, 1, 1, llm.Message{
		Role:             "assistant",
		Content:          "secret answer",
		ProviderMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("InsertMessage(assistant) error: %v", err)
	}
	var originalPayload string
	if err := database.QueryRow(`SELECT payload FROM message_provider_outputs WHERE message_id = ?`, assistantID).Scan(&originalPayload); err != nil {
		t.Fatalf("read raw provider payload: %v", err)
	}
	if _, err := database.Exec(`UPDATE message_provider_outputs SET payload = '{"plain":"tamper"}' WHERE message_id = ?`, assistantID); err != nil {
		t.Fatalf("tamper provider payload: %v", err)
	}
	if _, err := GetMessages(ctx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("plain provider payload error = %v, want ErrStorageDecryption", err)
	}
	if _, err := database.Exec(`UPDATE message_provider_outputs SET payload = ? WHERE message_id = ?`, originalPayload, assistantID); err != nil {
		t.Fatalf("restore provider payload: %v", err)
	}

	if _, err := database.Exec(`UPDATE messages SET content = 'plain tamper' WHERE id = ?`, messageID); err != nil {
		t.Fatalf("tamper message content: %v", err)
	}
	if _, err := GetMessages(ctx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("plaintext content error = %v, want ErrStorageDecryption", err)
	}

	if _, err := database.Exec(`UPDATE messages SET content = 'enc:v1:not-base64!' WHERE id = ?`, messageID); err != nil {
		t.Fatalf("tamper message envelope: %v", err)
	}
	if _, err := GetMessages(ctx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("malformed content error = %v, want ErrStorageDecryption", err)
	}

	if _, err := database.Exec(`UPDATE chats SET title = 'plain title' WHERE id = 1`); err != nil {
		t.Fatalf("tamper chat title: %v", err)
	}
	if _, err := GetChatTitle(ctx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("plaintext title error = %v, want ErrStorageDecryption", err)
	}
}
