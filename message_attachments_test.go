package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func testPDFBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
}

func TestMessageHandlerPersistsPDFAttachmentFromContentSignature(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 71)
	pdf := testPDFBytes()
	body, contentType := multipartImageRequest(t, "", []testUpload{{`..\\nested\\report.pdf`, "text/plain", pdf}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=71", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 71)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	attachment := messages[0].Attachments[0]
	if attachment.Filename != "report.pdf" || attachment.MediaType != "application/pdf" || attachment.Width != 0 || attachment.Height != 0 || !bytes.Equal(attachment.Data, pdf) {
		t.Errorf("stored PDF = %#v", attachment)
	}
}

func TestMessageHandlerPreservesMixedAttachmentOrder(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 72)
	jpegBytes := testImageBytes(t, "jpeg", 4, 5)
	pngBytes := testImageBytes(t, "png", 2, 3)
	webpBytes := testWebPBytes(t)
	pdf := testPDFBytes()
	body, contentType := multipartImageRequest(t, "ordered", []testUpload{
		{"first.jpg", "image/jpeg", jpegBytes},
		{"second.pdf", "application/octet-stream", pdf},
		{"third.webp", "image/webp", webpBytes},
		{"fourth.png", "text/plain", pngBytes},
	})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=72", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 72)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	attachments := messages[0].Attachments
	wantNames := []string{"first.jpg", "second.pdf", "third.webp", "fourth.png"}
	wantTypes := []string{"image/jpeg", "application/pdf", "image/webp", "image/png"}
	for index := range wantNames {
		if attachments[index].Filename != wantNames[index] || attachments[index].MediaType != wantTypes[index] {
			t.Errorf("attachment %d = %q %q, want %q %q", index, attachments[index].Filename, attachments[index].MediaType, wantNames[index], wantTypes[index])
		}
	}
	if attachments[1].Width != 0 || attachments[1].Height != 0 || !bytes.Equal(attachments[1].Data, pdf) {
		t.Errorf("stored PDF = %#v", attachments[1])
	}
}

func TestMessageHandlerRejectsInvalidPDFContents(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 73)
	body, contentType := multipartImageRequest(t, "", []testUpload{{"fake.pdf", "application/pdf", []byte("not a PDF")}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=73", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 73)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("persisted messages = %d, want 0", len(messages))
	}
}

func TestMessageHandlerRejectsAttachmentsOnSlashCommands(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 74)
	body, contentType := multipartImageRequest(t, "/new", []testUpload{{"report.pdf", "application/pdf", testPDFBytes()}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=74", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Slash commands cannot include attachments.")
	messages, err := kritui_db.GetMessages(context.Background(), database, 74)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("persisted messages = %d, want 0", len(messages))
	}
}

func TestMessageEditHandlerRejectsAttachments(t *testing.T) {
	database := openTestDatabase(t)
	mux := http.NewServeMux()
	mux.Handle("PUT /chats/{chat}/messages/{message}", messageEditHandler(database, newTestToolRegistry(t), newToolCallStore()))
	body, contentType := multipartImageRequest(t, "edited", []testUpload{{"report.pdf", "application/pdf", testPDFBytes()}})
	request := httptest.NewRequest(http.MethodPut, "/chats/1/messages/2", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Attachments cannot be added while editing a message.")
}

func TestMessageHandlerRejectsMixedAttachmentLimits(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 75)
	pngBytes := testImageBytes(t, "png", 1, 1)
	pdf := testPDFBytes()
	body, contentType := multipartImageRequest(t, "", []testUpload{
		{"1.png", "image/png", pngBytes},
		{"2.pdf", "application/pdf", pdf},
		{"3.png", "image/png", pngBytes},
		{"4.pdf", "application/pdf", pdf},
		{"5.png", "image/png", pngBytes},
	})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=75", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 75)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("persisted messages = %d, want 0", len(messages))
	}
}

func TestMessageHandlerRejectsOversizedPDFAttachment(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 76)
	oversized := append([]byte("%PDF-"), bytes.Repeat([]byte("x"), int(maxAttachmentBytes))...)
	body, contentType := multipartImageRequest(t, "", []testUpload{{"large.pdf", "application/pdf", oversized}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=76", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
}

func TestMessageHandlerPDFOnlySkipsImageCapabilityCheck(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 77)
	var modelRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			modelRequests.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"vision-model","capabilities":{"image_input":{"supported":false}},"architecture":{"input_modalities":["text"]}}]}`)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	body, contentType := multipartImageRequest(t, "Review this PDF", []testUpload{{"report.pdf", "application/pdf", testPDFBytes()}})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=77", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if modelRequests.Load() != 0 {
		t.Errorf("model discovery requests = %d, want 0 for PDF-only upload", modelRequests.Load())
	}
	messages, err := kritui_db.GetMessages(context.Background(), database, 77)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 1 || messages[0].Attachments[0].MediaType != "application/pdf" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestMessageHandlerMixedAttachmentsStillCheckImageCapability(t *testing.T) {
	database := openTestDatabase(t)
	seedCurrentChat(t, database, 78)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"vision-model","capabilities":{"image_input":{"supported":false}},"architecture":{"input_modalities":["text"]}}]}`)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	body, contentType := multipartImageRequest(t, "Review these files", []testUpload{
		{"report.pdf", "application/pdf", testPDFBytes()},
		{"diagram.png", "image/png", testImageBytes(t, "png", 1, 1)},
	})
	request := httptest.NewRequest(http.MethodPost, "/messages?chat=78", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	messageHandler(database, newTestToolRegistry(t), newTestCommandRegistry(t, database), newToolCallStore())(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
	}
	requireContains(t, response.Body.String(), "Selected model does not support images.")
	messages, err := kritui_db.GetMessages(context.Background(), database, 78)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("persisted messages = %d, want 0", len(messages))
	}
}

func TestMessageCompletionHandlerPDFOnlySkipsImageCapabilityCheck(t *testing.T) {
	database := openTestDatabase(t)
	var modelRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			modelRequests.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"vision-model","capabilities":{"image_input":{"supported":false}},"architecture":{"input_modalities":["text"]}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"vision-model","choices":[{"message":{"role":"assistant","content":"Answer"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := kritui_db.InsertMessage(context.Background(), database, 1, 0, llm.Message{
		Role:        "user",
		Content:     "Review this PDF",
		Attachments: []llm.UserAttachment{{Filename: "report.pdf", MediaType: "application/pdf", Data: testPDFBytes()}},
	}); err != nil {
		t.Fatalf("insert PDF message: %v", err)
	}
	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls, "/messages/complete?chat=1", url.Values{
		"model":   {"vision-model"},
		"request": {newToolCallRequest(t, toolCalls, 1)},
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Answer") {
		t.Errorf("completion body = %q, want rendered answer", response.Body.String())
	}
	if modelRequests.Load() != 0 {
		t.Errorf("model discovery requests = %d, want 0 for PDF-only completion", modelRequests.Load())
	}
	var assistants int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = 1 AND role = 'assistant'`).Scan(&assistants); err != nil {
		t.Fatal(err)
	}
	if assistants != 1 {
		t.Errorf("stored assistants = %d, want 1", assistants)
	}
}

func seedAttachmentMessage(t *testing.T, database *sql.DB, ctx context.Context, attachments []llm.UserAttachment) (int64, int64) {
	t.Helper()

	chatID, err := kritui_db.AllocateChat(ctx, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate attachment chat: %v", err)
	}
	messageID, err := kritui_db.InsertMessage(ctx, database, chatID, 0, llm.Message{
		Role:        "user",
		Content:     "Attachment fixture",
		Attachments: attachments,
	})
	if err != nil {
		t.Fatalf("insert attachment message: %v", err)
	}
	return chatID, messageID
}

func serveAttachmentRequest(t *testing.T, handler http.Handler, ctx context.Context, method, chat, message, position string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, "/chats/"+chat+"/messages/"+message+"/attachments/"+position, nil)
	request = request.WithContext(ctx)
	request.SetPathValue("chat", chat)
	request.SetPathValue("message", message)
	request.SetPathValue("position", position)
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireInlinePDFDisposition(t *testing.T, header, wantFilename string) {
	t.Helper()

	if strings.ContainsAny(header, "\r\n") {
		t.Fatalf("Content-Disposition contains a line break: %q", header)
	}
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil {
		t.Fatalf("parse Content-Disposition %q: %v", header, err)
	}
	if mediaType != "inline" {
		t.Errorf("Content-Disposition media type = %q, want inline", mediaType)
	}
	if params["filename"] != wantFilename {
		t.Errorf("Content-Disposition filename = %q, want %q", params["filename"], wantFilename)
	}
}

func requireAttachmentNotLeaked(t *testing.T, response *httptest.ResponseRecorder, values ...string) {
	t.Helper()
	requireNotContains(t, response.Body.String(), values...)
	requireNotContains(t, response.Header().Get("Content-Disposition"), values...)
}

func TestMessageAttachmentHandlerServesPDF(t *testing.T) {
	database := openTestDatabase(t)
	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	const filename = `report "final" (1).pdf`
	pdf := testPDFBytes()
	chatID, messageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{{
		Filename: filename, MediaType: "application/pdf", Data: pdf,
	}})
	handler := messageAttachmentHandler(database)
	chat := strconv.FormatInt(chatID, 10)
	message := strconv.FormatInt(messageID, 10)

	t.Run("GET returns the inline PDF", func(t *testing.T) {
		response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet, chat, message, "0", nil)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q, want application/pdf", got)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if got := response.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges = %q, want bytes", got)
		}
		requireInlinePDFDisposition(t, response.Header().Get("Content-Disposition"), filename)
		if !bytes.Equal(response.Body.Bytes(), pdf) {
			t.Errorf("body = %q, want the stored PDF", response.Body.String())
		}
	})

	t.Run("HEAD returns headers without a body", func(t *testing.T) {
		response := serveAttachmentRequest(t, handler, ownerContext, http.MethodHead, chat, message, "0", nil)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
		}
		if response.Body.Len() != 0 {
			t.Errorf("HEAD body length = %d, want 0", response.Body.Len())
		}
		if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(pdf)) {
			t.Errorf("Content-Length = %q, want %d", got, len(pdf))
		}
		if got := response.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q, want application/pdf", got)
		}
		requireInlinePDFDisposition(t, response.Header().Get("Content-Disposition"), filename)
	})

	t.Run("Range returns a partial PDF", func(t *testing.T) {
		response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet, chat, message, "0", map[string]string{
			"Range": "bytes=0-4",
		})

		if response.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusPartialContent, response.Body.String())
		}
		if got := response.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-4/%d", len(pdf)) {
			t.Errorf("Content-Range = %q, want bytes 0-4/%d", got, len(pdf))
		}
		if got := response.Body.String(); got != "%PDF-" {
			t.Errorf("partial body = %q, want %%PDF-", got)
		}
		if got := response.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q, want application/pdf", got)
		}
	})

	t.Run("unsatisfiable Range is rejected", func(t *testing.T) {
		response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet, chat, message, "0", map[string]string{
			"Range": "bytes=100000-100004",
		})

		if response.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusRequestedRangeNotSatisfiable, response.Body.String())
		}
		if got := response.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", len(pdf)) {
			t.Errorf("Content-Range = %q, want bytes */%d", got, len(pdf))
		}
		requireAttachmentNotLeaked(t, response, filename, "PRIVATE-ATTACHMENT-BYTES")
	})
}

func TestMessageAttachmentHandlerServesPDFAtMixedAttachmentIndex(t *testing.T) {
	database := openTestDatabase(t)
	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	png := testImageBytes(t, "png", 2, 2)
	pdf := testPDFBytes()
	firstImage := "first-image.png"
	pdfName := "second-report.pdf"
	thirdImage := "third-image.png"
	chatID, messageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{
		{Filename: firstImage, MediaType: "image/png", Width: 2, Height: 2, Data: png},
		{Filename: pdfName, MediaType: "application/pdf", Data: pdf},
		{Filename: thirdImage, MediaType: "image/png", Width: 2, Height: 2, Data: png},
	})
	handler := messageAttachmentHandler(database)
	chat := strconv.FormatInt(chatID, 10)
	message := strconv.FormatInt(messageID, 10)

	response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet, chat, message, "1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mixed PDF status = %d, want %d; body = %q", response.Code, http.StatusOK, response.Body.String())
	}
	requireInlinePDFDisposition(t, response.Header().Get("Content-Disposition"), pdfName)
	if !bytes.Equal(response.Body.Bytes(), pdf) {
		t.Error("mixed PDF body does not match the stored PDF")
	}

	for _, position := range []string{"0", "2"} {
		t.Run("image at "+position, func(t *testing.T) {
			response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet, chat, message, position, nil)
			if response.Code != http.StatusNotFound {
				t.Fatalf("image attachment status = %d, want %d; body = %q", response.Code, http.StatusNotFound, response.Body.String())
			}
			requireAttachmentNotLeaked(t, response, firstImage, pdfName, thirdImage)
		})
	}
}

func TestMessageAttachmentHandlerHidesForeignHiddenAndMissingAttachments(t *testing.T) {
	database := openTestDatabase(t)
	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	privatePDFName := "private-owner-report.pdf"
	privatePDF := []byte("%PDF-1.7\nPRIVATE-ATTACHMENT-BYTES\n%%EOF\n")
	chatID, messageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{{
		Filename: privatePDFName, MediaType: "application/pdf", Data: privatePDF,
	}})

	wrongChatID, err := kritui_db.AllocateChat(ownerContext, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate wrong chat: %v", err)
	}

	hiddenChatID, hiddenMessageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{{
		Filename: "hidden-report.pdf", MediaType: "application/pdf", Data: privatePDF,
	}})
	if _, err := kritui_db.UndoLatestTurn(ownerContext, database, hiddenChatID); err != nil {
		t.Fatalf("hide attachment message: %v", err)
	}

	imageName := "private-image.png"
	imageChatID, imageMessageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{{
		Filename: imageName, MediaType: "image/png", Width: 1, Height: 1, Data: testImageBytes(t, "png", 1, 1),
	}})

	foreign, err := kritui_db.CreateUser(context.Background(), database, "Attachment_Foreign", "encoded-attachment-foreign")
	if err != nil {
		t.Fatalf("create foreign account: %v", err)
	}
	foreignContext := kritui_db.WithUser(context.Background(), foreign.ID)

	handler := messageAttachmentHandler(database)
	tests := []struct {
		name     string
		ctx      context.Context
		chatID   int64
		message  int64
		position string
	}{
		{name: "foreign owner", ctx: foreignContext, chatID: chatID, message: messageID, position: "0"},
		{name: "wrong chat", ctx: ownerContext, chatID: wrongChatID, message: messageID, position: "0"},
		{name: "hidden by undo", ctx: ownerContext, chatID: hiddenChatID, message: hiddenMessageID, position: "0"},
		{name: "missing attachment slot", ctx: ownerContext, chatID: chatID, message: messageID, position: "1"},
		{name: "missing message", ctx: ownerContext, chatID: chatID, message: messageID + 1000, position: "0"},
		{name: "missing chat", ctx: ownerContext, chatID: chatID + 1000, message: messageID, position: "0"},
		{name: "image attachment slot", ctx: ownerContext, chatID: imageChatID, message: imageMessageID, position: "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveAttachmentRequest(t, handler, test.ctx, http.MethodGet,
				strconv.FormatInt(test.chatID, 10), strconv.FormatInt(test.message, 10), test.position, nil)

			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusNotFound, response.Body.String())
			}
			requireAttachmentNotLeaked(t, response, privatePDFName, "PRIVATE-ATTACHMENT-BYTES", imageName)
		})
	}
}

func TestMessageAttachmentHandlerRejectsInvalidPathValues(t *testing.T) {
	database := openTestDatabase(t)
	ownerContext := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	privatePDFName := "invalid-path-report.pdf"
	privatePDF := []byte("%PDF-1.7\nINVALID-PATH-PRIVATE-BYTES\n%%EOF\n")
	chatID, messageID := seedAttachmentMessage(t, database, ownerContext, []llm.UserAttachment{{
		Filename: privatePDFName, MediaType: "application/pdf", Data: privatePDF,
	}})
	handler := messageAttachmentHandler(database)
	chat := strconv.FormatInt(chatID, 10)
	message := strconv.FormatInt(messageID, 10)

	tests := []struct {
		name          string
		chat, message string
		position      string
	}{
		{name: "nonnumeric chat", chat: "not-a-chat", message: message, position: "0"},
		{name: "zero chat", chat: "0", message: message, position: "0"},
		{name: "negative chat", chat: "-1", message: message, position: "0"},
		{name: "nonnumeric message", chat: chat, message: "not-a-message", position: "0"},
		{name: "zero message", chat: chat, message: "0", position: "0"},
		{name: "negative message", chat: chat, message: "-1", position: "0"},
		{name: "negative position", chat: chat, message: message, position: "-1"},
		{name: "nonnumeric position", chat: chat, message: message, position: "not-a-position"},
		{name: "empty position", chat: chat, message: message, position: ""},
		{name: "fractional position", chat: chat, message: message, position: "1.5"},
		{name: "overflowing position", chat: chat, message: message, position: "999999999999999999999"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveAttachmentRequest(t, handler, ownerContext, http.MethodGet,
				test.chat, test.message, test.position, nil)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusBadRequest, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			requireAttachmentNotLeaked(t, response, privatePDFName, "INVALID-PATH-PRIVATE-BYTES")
		})
	}
}

func TestMessageCompletionIncludesActivePDFPromptLinks(t *testing.T) {
	database := openTestDatabase(t)
	ctx := kritui_db.WithUser(context.Background(), kritui_db.LegacyOwnerID)
	const chatID int64 = 79
	seedCurrentChat(t, database, chatID)

	pdf := testPDFBytes()
	png := testImageBytes(t, "png", 1, 1)
	hiddenID, err := kritui_db.InsertMessage(ctx, database, chatID, 0, llm.Message{
		Role:    "user",
		Content: "Hidden PDF turn",
		Attachments: []llm.UserAttachment{{
			Filename: "hidden-private.pdf", MediaType: "application/pdf", Data: pdf,
		}},
	})
	if err != nil {
		t.Fatalf("insert hidden PDF message: %v", err)
	}
	if _, err := kritui_db.UndoLatestTurn(ctx, database, chatID); err != nil {
		t.Fatalf("hide PDF message: %v", err)
	}

	earlierID, err := kritui_db.InsertMessage(ctx, database, chatID, 1, llm.Message{
		Role:    "user",
		Content: "Earlier files",
		Attachments: []llm.UserAttachment{
			{Filename: "earlier-image.png", MediaType: "image/png", Width: 1, Height: 1, Data: png},
			{Filename: "shared-report.pdf", MediaType: "application/pdf", Data: pdf},
			{Filename: "earlier-image-two.png", MediaType: "image/png", Width: 1, Height: 1, Data: png},
		},
	})
	if err != nil {
		t.Fatalf("insert earlier message: %v", err)
	}
	latestID, err := kritui_db.InsertMessage(ctx, database, chatID, 2, llm.Message{
		Role:    "user",
		Content: "Latest files",
		Attachments: []llm.UserAttachment{
			{Filename: "shared-report.pdf", MediaType: "application/pdf", Data: pdf},
			{Filename: "latest-image.png", MediaType: "image/png", Width: 1, Height: 1, Data: png},
		},
	})
	if err != nil {
		t.Fatalf("insert latest message: %v", err)
	}

	snapshot, err := kritui_db.GetMessageSnapshot(ctx, database, chatID)
	if err != nil {
		t.Fatalf("get active snapshot: %v", err)
	}
	if len(snapshot.Messages) != 2 || snapshot.Messages[0].ID != earlierID || snapshot.Messages[1].ID != latestID {
		t.Fatalf("active messages = %#v, want persisted IDs %d and %d", snapshot.Messages, earlierID, latestID)
	}

	type completionRequest struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	requests := make(chan completionRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"vision-model","capabilities":{"image_input":{"supported":true}}}]}`)
			return
		}
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request
		_, _ = io.WriteString(w, `{"model":"vision-model","choices":[{"message":{"role":"assistant","content":"Answer"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	t.Setenv("LLM_KEY", "test-key")
	t.Setenv("LLM_MODEL", "vision-model")
	t.Setenv("LLM_ENDPOINT", server.URL+"/v1/chat/completions")

	toolCalls := newToolCallStore()
	response := completeForm(t, messageCompletionHandler(database, newTestToolRegistry(t), toolCalls, nil), toolCalls,
		fmt.Sprintf("/messages/complete?chat=%d", chatID), url.Values{
			"model":   {"vision-model"},
			"request": {newToolCallRequest(t, toolCalls, chatID)},
		})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Answer") {
		t.Fatalf("completion status = %d, body = %q, want rendered answer", response.Code, response.Body.String())
	}

	request := <-requests
	var systemPrompt string
	for _, message := range request.Messages {
		if message.Role == "system" {
			systemPrompt, _ = message.Content.(string)
			break
		}
	}
	if systemPrompt == "" {
		t.Fatalf("provider request has no system prompt: %#v", request.Messages)
	}
	if count := strings.Count(systemPrompt, "## PDF attachments"); count != 1 {
		t.Errorf("PDF metadata section count = %d, want 1: %q", count, systemPrompt)
	}
	if count := strings.Count(systemPrompt, "\n- Filename:"); count != 2 {
		t.Errorf("PDF metadata entry count = %d, want 2: %q", count, systemPrompt)
	}

	earlierURL := fmt.Sprintf("/chats/%d/messages/%d/attachments/1", chatID, earlierID)
	latestURL := fmt.Sprintf("/chats/%d/messages/%d/attachments/0", chatID, latestID)
	hiddenURL := fmt.Sprintf("/chats/%d/messages/%d/attachments/0", chatID, hiddenID)
	requireContains(t, systemPrompt,
		fmt.Sprintf(`Filename: "shared-report.pdf"; URL: %s`, earlierURL),
		fmt.Sprintf(`Filename: "shared-report.pdf"; URL: %s`, latestURL),
	)
	requireNotContains(t, systemPrompt,
		hiddenURL,
		"hidden-private.pdf",
		fmt.Sprintf("/chats/%d/messages/%d/attachments/0", chatID, earlierID),
		fmt.Sprintf("/chats/%d/messages/%d/attachments/2", chatID, earlierID),
		fmt.Sprintf("/chats/%d/messages/%d/attachments/1", chatID, latestID),
		"earlier-image.png",
		"earlier-image-two.png",
		"latest-image.png",
	)
}

func TestPDFPromptLinksSkipsNonUserAndUnstoredMessages(t *testing.T) {
	pdf := llm.UserAttachment{Filename: "report.pdf", MediaType: "application/pdf", Data: testPDFBytes()}
	png := llm.UserAttachment{Filename: "image.png", MediaType: "image/png", Width: 1, Height: 1, Data: testImageBytes(t, "png", 1, 1)}
	messages := []llm.Message{
		{ID: 0, Role: "user", Attachments: []llm.UserAttachment{pdf}},
		{ID: 4, Role: "assistant", Attachments: []llm.UserAttachment{pdf}},
		{ID: 5, Role: "user", Attachments: []llm.UserAttachment{png}},
		{ID: 6, Role: "user", Attachments: []llm.UserAttachment{png, pdf}},
	}

	links := pdfPromptLinks(9, messages)
	if len(links) != 1 {
		t.Fatalf("pdfPromptLinks() = %#v, want one PDF link", links)
	}
	want := llm.PDFLink{Filename: "report.pdf", URL: "/chats/9/messages/6/attachments/1"}
	if links[0] != want {
		t.Errorf("pdfPromptLinks()[0] = %#v, want %#v", links[0], want)
	}
}

func TestMessageAttachmentRouteRequiresAuthentication(t *testing.T) {
	database := openTestDatabase(t)
	handler := newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		newToolCallStore(),
		nil,
		newAuthService(database, false),
	)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newAuthTestClient()

	const username = "Attachment_Route_User"
	_, cookie := authSignup(t, client, server.URL, username, authTestPassword, "")
	user, err := kritui_db.GetUserByUsername(context.Background(), database, username)
	if err != nil {
		t.Fatalf("look up signed-up attachment owner: %v", err)
	}
	userContext := kritui_db.WithUser(context.Background(), user.ID)
	privatePDF := []byte("%PDF-1.7\nROUTE-PRIVATE-ATTACHMENT-BYTES\n%%EOF\n")
	chatID, messageID := seedAttachmentMessage(t, database, userContext, []llm.UserAttachment{{
		Filename: "route-private-report.pdf", MediaType: "application/pdf", Data: privatePDF,
	}})
	path := fmt.Sprintf("/chats/%d/messages/%d/attachments/0", chatID, messageID)

	response, body := authGet(t, client, server.URL+path, nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated attachment status = %d, want %d; body = %q", response.StatusCode, http.StatusSeeOther, body)
	}
	if location := response.Header.Get("Location"); !strings.HasPrefix(location, "/login?next=") {
		t.Errorf("unauthenticated attachment Location = %q, want a login redirect", location)
	}
	requireNotContains(t, body, "ROUTE-PRIVATE-ATTACHMENT-BYTES", "route-private-report.pdf")

	response, body = authGet(t, client, server.URL+path, cookie, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated attachment status = %d, want %d; body = %q", response.StatusCode, http.StatusOK, body)
	}
	if body != string(privatePDF) {
		t.Errorf("authenticated attachment body = %q, want the stored PDF", body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	requireInlinePDFDisposition(t, response.Header.Get("Content-Disposition"), "route-private-report.pdf")
}
