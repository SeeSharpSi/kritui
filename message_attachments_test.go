package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
