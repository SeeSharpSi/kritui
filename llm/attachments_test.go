package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type attachmentProtocol struct {
	name     string
	suffix   string
	response string
}

func attachmentProtocols() []attachmentProtocol {
	return []attachmentProtocol{
		{
			name:     "responses",
			suffix:   "/v1/responses",
			response: `{"model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
		},
		{
			name:     "messages",
			suffix:   "/v1/chat/messages",
			response: `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		},
		{
			name:     "chat",
			suffix:   "/v1/chat/completions",
			response: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		},
	}
}

func completeAttachmentRequest(t *testing.T, protocol attachmentProtocol, messages []Message) (map[string]any, int) {
	t.Helper()

	var request map[string]any
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(protocol.response))
	}))
	defer server.Close()

	client, err := New("key", "model", server.URL+protocol.suffix)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if _, err := client.complete(context.Background(), messages, nil); err != nil {
		t.Fatalf("complete() error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	return request, calls
}

func attachmentRequestParts(t *testing.T, protocol attachmentProtocol, request map[string]any) []any {
	t.Helper()

	message := request
	if protocol.name == "responses" {
		input, ok := request["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input = %#v", request["input"])
		}
		message, ok = input[0].(map[string]any)
		if !ok {
			t.Fatalf("input item = %#v", input[0])
		}
	} else {
		messages, ok := request["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %#v", request["messages"])
		}
		message, ok = messages[0].(map[string]any)
		if !ok {
			t.Fatalf("message = %#v", messages[0])
		}
	}

	if message["role"] != "user" {
		t.Fatalf("role = %#v", message["role"])
	}
	parts, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v", message["content"])
	}
	return parts
}

func attachmentPart(protocol attachmentProtocol, attachment UserAttachment) map[string]any {
	encoded := base64.StdEncoding.EncodeToString(attachment.Data)
	if isPDFAttachment(attachment) {
		switch protocol.name {
		case "responses":
			return map[string]any{
				"type":      "input_file",
				"filename":  attachment.Filename,
				"file_data": "data:application/pdf;base64," + encoded,
			}
		case "messages":
			document := map[string]any{
				"type": "document",
				"source": map[string]any{
					"type":       "base64",
					"media_type": "application/pdf",
					"data":       encoded,
				},
			}
			if attachment.Filename != "" {
				document["title"] = attachment.Filename
			}
			return document
		default:
			return map[string]any{
				"type": "file",
				"file": map[string]any{
					"filename":  attachment.Filename,
					"file_data": "data:application/pdf;base64," + encoded,
				},
			}
		}
	}

	if protocol.name == "messages" {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": attachment.MediaType,
				"data":       encoded,
			},
		}
	}
	part := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:" + attachment.MediaType + ";base64," + encoded, "detail": "auto"},
	}
	if protocol.name == "responses" {
		part = map[string]any{
			"type":      "input_image",
			"image_url": "data:" + attachment.MediaType + ";base64," + encoded,
			"detail":    "auto",
		}
	}
	return part
}

func attachmentTextPart(protocol attachmentProtocol, text string) map[string]any {
	partType := "text"
	if protocol.name == "responses" {
		partType = "input_text"
	}
	return map[string]any{"type": partType, "text": text}
}

func TestCompletePDFAndMixedAttachmentSerializers(t *testing.T) {
	pdfData := []byte("%PDF-1.7\nattachment bytes\n")
	imageData := []byte{0x01, 0x02, 0x03}
	pdf := UserAttachment{Filename: "report.pdf", MediaType: "application/pdf", Data: pdfData}
	image := UserAttachment{Filename: "chart.png", MediaType: "image/png", Width: 4, Height: 5, Data: imageData}

	for _, protocol := range attachmentProtocols() {
		t.Run(protocol.name+"/mixed", func(t *testing.T) {
			request, _ := completeAttachmentRequest(t, protocol, []Message{{
				Role:        "user",
				Content:     "describe",
				Attachments: []UserAttachment{pdf, image},
			}})
			want := []any{
				attachmentPart(protocol, pdf),
				attachmentPart(protocol, image),
				attachmentTextPart(protocol, "describe"),
			}
			if parts := attachmentRequestParts(t, protocol, request); !reflect.DeepEqual(parts, want) {
				t.Fatalf("parts = %#v, want %#v", parts, want)
			}
		})

		t.Run(protocol.name+"/pdf_only", func(t *testing.T) {
			request, _ := completeAttachmentRequest(t, protocol, []Message{{
				Role:        "user",
				Attachments: []UserAttachment{pdf},
			}})
			want := []any{attachmentPart(protocol, pdf)}
			if parts := attachmentRequestParts(t, protocol, request); !reflect.DeepEqual(parts, want) {
				t.Fatalf("parts = %#v, want %#v", parts, want)
			}
		})

		t.Run(protocol.name+"/pdf_empty_filename", func(t *testing.T) {
			anonymous := UserAttachment{MediaType: "application/pdf", Data: pdfData}
			request, _ := completeAttachmentRequest(t, protocol, []Message{{
				Role:        "user",
				Attachments: []UserAttachment{anonymous},
			}})
			want := []any{attachmentPart(protocol, anonymous)}
			if parts := attachmentRequestParts(t, protocol, request); !reflect.DeepEqual(parts, want) {
				t.Fatalf("parts = %#v, want %#v", parts, want)
			}
		})
	}
}

func TestAttachmentsRejectedOnNonUserMessages(t *testing.T) {
	pdf := UserAttachment{Filename: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF")}
	image := UserAttachment{Filename: "chart.png", MediaType: "image/png", Data: []byte{1}}
	providerMetadata, err := NewResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prior"}]}`),
	})
	if err != nil {
		t.Fatalf("NewResponsesProviderMetadata() error: %v", err)
	}

	tests := []struct {
		name    string
		message Message
	}{
		{name: "assistant_pdf", message: Message{Role: "assistant", Attachments: []UserAttachment{pdf}}},
		{name: "assistant_image", message: Message{Role: "assistant", Attachments: []UserAttachment{image}}},
		{name: "tool_pdf", message: Message{Role: "tool", ToolCallID: "call_1", Attachments: []UserAttachment{pdf}}},
		{name: "assistant_provider_metadata_pdf", message: Message{Role: "assistant", Attachments: []UserAttachment{pdf}, ProviderMetadata: providerMetadata}},
	}
	for _, protocol := range attachmentProtocols() {
		for _, test := range tests {
			t.Run(protocol.name+"/"+test.name, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
				defer server.Close()

				client, err := New("key", "model", server.URL+protocol.suffix)
				if err != nil {
					t.Fatalf("New() error: %v", err)
				}
				_, err = client.complete(context.Background(), []Message{test.message}, nil)
				if err == nil || !strings.Contains(err.Error(), "only allowed on user messages") {
					t.Fatalf("error = %v", err)
				}
				if calls != 0 {
					t.Fatalf("calls = %d, want 0", calls)
				}
			})
		}
	}
}

func TestInvalidAttachmentsMakeNoRequest(t *testing.T) {
	for _, protocol := range attachmentProtocols() {
		for _, test := range []struct {
			name       string
			attachment UserAttachment
			wantError  string
		}{
			{
				name:       "unsupported_media_type",
				attachment: UserAttachment{Filename: "notes.txt", MediaType: "text/plain", Data: []byte("text")},
				wantError:  "unsupported attachment media type",
			},
			{
				name:       "empty_data",
				attachment: UserAttachment{Filename: "empty.pdf", MediaType: "application/pdf"},
				wantError:  "attachment data and media type are required",
			},
		} {
			t.Run(protocol.name+"/"+test.name, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
				defer server.Close()

				client, err := New("key", "model", server.URL+protocol.suffix)
				if err != nil {
					t.Fatalf("New() error: %v", err)
				}
				_, err = client.complete(context.Background(), []Message{{Role: "user", Attachments: []UserAttachment{test.attachment}}}, nil)
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				if calls != 0 {
					t.Fatalf("calls = %d, want 0", calls)
				}
			})
		}
	}
}
