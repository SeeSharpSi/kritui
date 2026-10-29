package kritui_db

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func encryptedInboxTestContext(t *testing.T) context.Context {
	t.Helper()
	return WithStorageCipher(WithUser(context.Background(), LegacyOwnerID), newTestStorageCipher(t))
}

func inboxEncryptedCiphertext(t *testing.T, stored string, plaintext string) {
	t.Helper()
	if stored == plaintext || (plaintext != "" && strings.Contains(stored, plaintext)) {
		t.Fatalf("stored value %q exposes plaintext %q", stored, plaintext)
	}
	if !strings.HasPrefix(stored, storageTextPrefix) {
		t.Fatalf("stored value %q lacks text envelope prefix", stored)
	}
}

func inboxEncryptedBlob(t *testing.T, stored []byte, plaintext []byte) {
	t.Helper()
	if bytes.Equal(stored, plaintext) || bytes.Contains(stored, plaintext) {
		t.Fatalf("stored blob %x exposes plaintext %x", stored, plaintext)
	}
	if len(stored) <= len(plaintext) {
		t.Fatalf("stored blob length = %d, want larger than plaintext length %d", len(stored), len(plaintext))
	}
}

func TestInboxEncryptionRoundTripsCiphertext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedInboxTestContext(t)
	insertMessagesTestUser(t, database)

	imageData := []byte{0x89, 'P', 'N', 'G', 0x01, 0x02}
	pdfData := []byte("%PDF-1.7\nencrypted attachment\n")
	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-round-trip",
		Delivery:     "queue",
		Model:        "plain-model",
		Tools:        []string{"git", "webfetch"},
		AppendIDs:    []string{"research"},
		Message: llm.Message{
			Role:              "user",
			Content:           "secret prompt body",
			PromptAppendTexts: []string{"secret append one", "secret append two"},
			Attachments: []llm.UserAttachment{
				{Filename: "secret-image.png", MediaType: "image/png", Width: 2, Height: 3, Data: imageData},
				{Filename: "secret-report.pdf", MediaType: "application/pdf", Data: pdfData},
			},
		},
	})
	if item.Message.Content != "secret prompt body" {
		t.Fatalf("enqueue result content = %q, want plaintext", item.Message.Content)
	}

	var (
		storedContent string
		storedModel   string
		storedStatus  string
	)
	if err := database.QueryRow(`
		SELECT content, model, status FROM inbox_items WHERE id = ?
	`, item.ID).Scan(&storedContent, &storedModel, &storedStatus); err != nil {
		t.Fatalf("read raw inbox item: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, item.Message.Content)
	if storedModel != "plain-model" || storedStatus != "pending" {
		t.Fatalf("raw metadata = %q %q, want plaintext model and status", storedModel, storedStatus)
	}

	rows, err := database.Query(`
		SELECT position, text FROM inbox_prompt_append_texts
		WHERE inbox_id = ?
		ORDER BY position
	`, item.ID)
	if err != nil {
		t.Fatalf("read raw append texts: %v", err)
	}
	defer rows.Close()
	var appendTexts []string
	for rows.Next() {
		var (
			position int
			stored   string
		)
		if err := rows.Scan(&position, &stored); err != nil {
			t.Fatalf("scan raw append text: %v", err)
		}
		plaintext := item.Message.PromptAppendTexts[position]
		inboxEncryptedCiphertext(t, stored, plaintext)
		appendTexts = append(appendTexts, stored)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate raw append texts: %v", err)
	}
	if len(appendTexts) != 2 {
		t.Fatalf("raw append text count = %d, want 2", len(appendTexts))
	}
	rows.Close()

	rows, err = database.Query(`
		SELECT position, filename, media_type, width, height, data
		FROM inbox_attachments
		WHERE inbox_id = ?
		ORDER BY position
	`, item.ID)
	if err != nil {
		t.Fatalf("read raw attachments: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var (
			position  int
			filename  string
			mediaType string
			width     int
			height    int
			data      []byte
		)
		if err := rows.Scan(&position, &filename, &mediaType, &width, &height, &data); err != nil {
			t.Fatalf("scan raw attachment: %v", err)
		}
		attachment := item.Message.Attachments[position]
		inboxEncryptedCiphertext(t, filename, attachment.Filename)
		inboxEncryptedBlob(t, data, attachment.Data)
		if mediaType != attachment.MediaType || width != attachment.Width || height != attachment.Height {
			t.Fatalf("raw attachment metadata = %q %d %d, want plaintext metadata", mediaType, width, height)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate raw attachments: %v", err)
	}
	if count != 2 {
		t.Fatalf("raw attachment count = %d, want 2", count)
	}
	rows.Close()

	pending, err := GetInbox(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetInbox() error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("GetInbox() length = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.Message.Content != item.Message.Content {
		t.Fatalf("decrypted content = %q, want %q", got.Message.Content, item.Message.Content)
	}
	if !slices.Equal(got.Message.PromptAppendTexts, item.Message.PromptAppendTexts) {
		t.Fatalf("decrypted append texts = %v, want %v", got.Message.PromptAppendTexts, item.Message.PromptAppendTexts)
	}
	if !slices.EqualFunc(got.Message.Attachments, item.Message.Attachments, func(a, b llm.UserAttachment) bool {
		return a.Filename == b.Filename && a.MediaType == b.MediaType && a.Width == b.Width && a.Height == b.Height && slices.Equal(a.Data, b.Data)
	}) {
		t.Fatalf("decrypted attachments = %#v, want %#v", got.Message.Attachments, item.Message.Attachments)
	}
	if !slices.Equal(got.Tools, item.Tools) || !slices.Equal(got.AppendIDs, item.AppendIDs) {
		t.Fatalf("plaintext selections = %v %v, want %v %v", got.Tools, got.AppendIDs, item.Tools, item.AppendIDs)
	}
}

func TestInboxEncryptionHandlesEmptyContentWithAttachment(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedInboxTestContext(t)
	insertMessagesTestUser(t, database)

	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-attachment-only",
		Delivery:     "steer",
		Message: llm.Message{
			Role:        "user",
			Attachments: []llm.UserAttachment{{Filename: "only.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.7\n")}},
		},
	})

	var storedContent string
	if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, item.ID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw content: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, "")

	pending, err := GetInbox(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetInbox() error: %v", err)
	}
	if len(pending) != 1 || pending[0].Message.Content != "" {
		t.Fatalf("attachment-only content = %#v, want empty string", pending)
	}
	if len(pending[0].Message.Attachments) != 1 || pending[0].Message.Attachments[0].Filename != "only.pdf" {
		t.Fatalf("attachment-only attachments = %#v", pending[0].Message.Attachments)
	}
}

func TestInboxEncryptionDedupeAndCancelledReceiptStayCiphertext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedInboxTestContext(t)
	insertMessagesTestUser(t, database)

	original := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-receipt",
		Delivery:     "queue",
		Message: llm.Message{
			Role:              "user",
			Content:           "receipt secret",
			PromptAppendTexts: []string{"receipt append"},
		},
	})
	replay := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-receipt",
		Delivery:     "steer",
		Message:      llm.Message{Role: "user", Content: "different replay body"},
	})
	if replay.ID != original.ID || replay.Message.Content != original.Message.Content || replay.Delivery != original.Delivery {
		t.Fatalf("replay receipt = %#v, want original %#v", replay, original)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM inbox_items`).Scan(&count); err != nil {
		t.Fatalf("count inbox items: %v", err)
	}
	if count != 1 {
		t.Fatalf("inbox item count = %d, want 1", count)
	}

	var storedContent string
	if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, original.ID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw content: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, original.Message.Content)

	if err := CancelInboxPrompt(ctx, database, 1, original.ID); err != nil {
		t.Fatalf("CancelInboxPrompt() error: %v", err)
	}
	if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, original.ID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw cancelled content: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, original.Message.Content)

	receipt := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-receipt",
		Delivery:     "steer",
		Message:      llm.Message{Role: "user", Content: "ignored"},
	})
	if receipt.Status != "cancelled" || receipt.Message.Content != original.Message.Content {
		t.Fatalf("cancelled receipt = %#v, want decrypted original content", receipt)
	}
	if len(receipt.Message.PromptAppendTexts) != 0 || len(receipt.Message.Attachments) != 0 {
		t.Fatalf("cancelled receipt retained discarded children: %#v", receipt.Message)
	}
}

func TestInboxEncryptionPromotionDoesNotCopyCiphertext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedInboxTestContext(t)
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}

	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-promotion",
		Delivery:     "queue",
		Model:        "promotion-model",
		Tools:        []string{"git"},
		AppendIDs:    []string{"research"},
		Message: llm.Message{
			Role:              "user",
			Content:           "promoted secret body",
			PromptAppendTexts: []string{"promoted append"},
			Attachments:       []llm.UserAttachment{{Filename: "promoted.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.7\npromoted\n")}},
		},
	})
	var storedContent string
	if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, item.ID).Scan(&storedContent); err != nil {
		t.Fatalf("read raw inbox content: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, item.Message.Content)

	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "answer"}}, "input")
	if err != nil {
		t.Fatalf("CheckpointCompletion() error: %v", err)
	}
	if len(result.Delivered) != 1 {
		t.Fatalf("delivered count = %d, want 1", len(result.Delivered))
	}
	delivered := result.Delivered[0]
	if delivered.Message.Content != item.Message.Content {
		t.Fatalf("delivered content = %q, want plaintext %q", delivered.Message.Content, item.Message.Content)
	}
	if !slices.Equal(delivered.Message.PromptAppendTexts, item.Message.PromptAppendTexts) {
		t.Fatalf("delivered append texts = %v, want %v", delivered.Message.PromptAppendTexts, item.Message.PromptAppendTexts)
	}

	if err := database.QueryRow(`SELECT content FROM inbox_items WHERE id = ?`, item.ID).Scan(&storedContent); err != nil {
		t.Fatalf("read delivered raw inbox content: %v", err)
	}
	inboxEncryptedCiphertext(t, storedContent, item.Message.Content)

	var rawMessageContent string
	if err := database.QueryRow(`SELECT content FROM messages WHERE id = ?`, delivered.Message.ID).Scan(&rawMessageContent); err != nil {
		t.Fatalf("read raw message content: %v", err)
	}
	if rawMessageContent == storedContent || strings.Contains(rawMessageContent, storedContent) {
		t.Fatalf("promoted message copied inbox ciphertext %q", rawMessageContent)
	}
	if !strings.HasPrefix(rawMessageContent, storageTextPrefix) {
		t.Fatalf("promoted message content %q is not encrypted", rawMessageContent)
	}
	promotedContent, err := DecryptStorageText(ctx, storageScopeMessageContent, messageStorageRowID(delivered.Message.ID), rawMessageContent)
	if err != nil {
		t.Fatalf("decrypt promoted message content: %v", err)
	}
	if promotedContent != item.Message.Content {
		t.Fatalf("promoted message content = %q, want plaintext %q", promotedContent, item.Message.Content)
	}

	var (
		rawAppendText string
		rawFilename   string
		rawData       []byte
	)
	if err := database.QueryRow(`
		SELECT text FROM message_prompt_appends WHERE message_id = ? AND position = 0
	`, delivered.Message.ID).Scan(&rawAppendText); err != nil {
		t.Fatalf("read raw promoted append: %v", err)
	}
	if rawAppendText != item.Message.PromptAppendTexts[0] && !strings.HasPrefix(rawAppendText, storageTextPrefix) {
		t.Fatalf("promoted append text %q is neither plaintext nor encrypted", rawAppendText)
	}
	if strings.HasPrefix(rawAppendText, storageTextPrefix) {
		appendText, err := DecryptStorageText(ctx, storageScopePromptAppendText, messageChildStorageRowID(delivered.Message.ID, 0), rawAppendText)
		if err != nil {
			t.Fatalf("decrypt promoted append text: %v", err)
		}
		if appendText != item.Message.PromptAppendTexts[0] {
			t.Fatalf("promoted append text = %q, want %q", appendText, item.Message.PromptAppendTexts[0])
		}
	}
	if err := database.QueryRow(`
		SELECT filename, data FROM message_user_images WHERE message_id = ? AND position = 0
	`, delivered.Message.ID).Scan(&rawFilename, &rawData); err != nil {
		t.Fatalf("read raw promoted attachment: %v", err)
	}
	if !strings.HasPrefix(rawFilename, storageTextPrefix) {
		t.Fatalf("promoted attachment filename %q is not encrypted", rawFilename)
	}
	filename, err := DecryptStorageText(ctx, storageScopeAttachmentFilename, messageChildStorageRowID(delivered.Message.ID, 0), rawFilename)
	if err != nil {
		t.Fatalf("decrypt promoted attachment filename: %v", err)
	}
	if filename != "promoted.pdf" {
		t.Fatalf("promoted attachment filename = %q, want promoted.pdf", filename)
	}
	data, err := DecryptStorageBytes(ctx, storageScopeAttachmentData, messageChildStorageRowID(delivered.Message.ID, 0), rawData)
	if err != nil {
		t.Fatalf("decrypt promoted attachment data: %v", err)
	}
	if !bytes.Equal(data, []byte("%PDF-1.7\npromoted\n")) {
		t.Fatalf("promoted attachment data = %q", data)
	}
}

func TestInboxEncryptionRejectsForeignCipherContext(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := encryptedInboxTestContext(t)
	insertMessagesTestUser(t, database)
	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "encrypted-owner",
		Delivery:     "queue",
		Message:      llm.Message{Role: "user", Content: "owner secret"},
	})

	otherCipher, err := NewStorageCipher(bytes.Repeat([]byte{7}, StorageKeySize))
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	otherCtx := WithStorageCipher(WithUser(context.Background(), LegacyOwnerID), otherCipher)
	if _, err := GetInbox(otherCtx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("GetInbox() with foreign cipher error = %v, want ErrStorageDecryption", err)
	}

	if _, err := database.Exec(`UPDATE inbox_items SET content = 'plaintext tamper' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("tamper raw content: %v", err)
	}
	if _, err := GetInbox(ctx, database, 1); !errors.Is(err, ErrStorageDecryption) {
		t.Fatalf("GetInbox() with plaintext row error = %v, want ErrStorageDecryption", err)
	}
}
