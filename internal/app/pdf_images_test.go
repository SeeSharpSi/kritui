package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestPrepareMessagesForModelConvertsPDFPages(t *testing.T) {
	pdf := llm.UserAttachment{
		Filename:  "Nemesis Rulebook.pdf",
		MediaType: "application/pdf",
		Data:      testPDFDocumentBytes(t),
	}
	messages := []llm.Message{{
		Role:        "user",
		Content:     "What page says how escape pods work?",
		Attachments: []llm.UserAttachment{pdf},
	}}

	prepared, err := prepareMessagesForModel(context.Background(), messages, llm.ModelInfo{
		ID:           "vision-model",
		ImageSupport: llm.InputSupportSupported,
		PDFSupport:   llm.InputSupportUnsupported,
	})
	if err != nil {
		t.Fatalf("prepareMessagesForModel() error: %v", err)
	}
	if len(prepared) != 1 || len(prepared[0].Attachments) != 2 {
		t.Fatalf("prepared messages = %#v, want one message with two page images", prepared)
	}
	for index, attachment := range prepared[0].Attachments {
		wantName := "Nemesis Rulebook.pdf - page " + []string{"1", "2"}[index] + ".jpg"
		if attachment.Filename != wantName {
			t.Errorf("page %d filename = %q, want %q", index+1, attachment.Filename, wantName)
		}
		if attachment.MediaType != "image/jpeg" || attachment.Width <= 0 || attachment.Height <= 0 || len(attachment.Data) == 0 {
			t.Errorf("page %d attachment = %#v", index+1, attachment)
		}
	}
	if len(prepared[0].PromptAppendTexts) != 1 || prepared[0].PromptAppendTexts[0] != "You've received a PDF that has been converted to images." {
		t.Errorf("prompt appends = %#v", prepared[0].PromptAppendTexts)
	}
	if len(messages[0].Attachments) != 1 || messages[0].Attachments[0].MediaType != "application/pdf" {
		t.Fatalf("source message was modified: %#v", messages[0].Attachments)
	}
}

func TestPrepareMessagesForModelRejectsPDFWhenImagesUnsupported(t *testing.T) {
	messages := []llm.Message{{
		Role:    "user",
		Content: "Review this PDF",
		Attachments: []llm.UserAttachment{{
			Filename:  "report.pdf",
			MediaType: "application/pdf",
			Data:      testPDFDocumentBytes(t),
		}},
	}}

	_, err := prepareMessagesForModel(context.Background(), messages, llm.ModelInfo{
		ID:           "text-model",
		ImageSupport: llm.InputSupportUnsupported,
		PDFSupport:   llm.InputSupportUnsupported,
	})
	if !errors.Is(err, errModelDoesNotSupportPDFsOrImages) {
		t.Fatalf("error = %v, want unsupported PDF and image error", err)
	}
}

func TestPrepareMessagesForModelLeavesPDFForPDFCapableModel(t *testing.T) {
	messages := []llm.Message{{
		Role:    "user",
		Content: "Review this PDF",
		Attachments: []llm.UserAttachment{{
			Filename:  "report.pdf",
			MediaType: "application/pdf",
			Data:      testPDFDocumentBytes(t),
		}},
	}}

	prepared, err := prepareMessagesForModel(context.Background(), messages, llm.ModelInfo{
		ID:           "document-model",
		ImageSupport: llm.InputSupportUnsupported,
		PDFSupport:   llm.InputSupportSupported,
	})
	if err != nil {
		t.Fatalf("prepareMessagesForModel() error: %v", err)
	}
	if len(prepared[0].Attachments) != 1 || prepared[0].Attachments[0].MediaType != "application/pdf" {
		t.Fatalf("prepared attachments = %#v, want native PDF", prepared[0].Attachments)
	}
}

func TestPDFPageFilenameKeepsPDFNameAndPage(t *testing.T) {
	if got, want := pdfPageFilename(`C:\docs\Rulebook.pdf`, 12), "Rulebook.pdf - page 12.jpg"; got != want {
		t.Errorf("pdfPageFilename() = %q, want %q", got, want)
	}
	long := strings.Repeat("a", 300) + ".pdf"
	got := pdfPageFilename(long, 7)
	if len([]rune(got)) != 255 {
		t.Errorf("long filename length = %d, want 255", len([]rune(got)))
	}
	if !strings.HasSuffix(got, " - page 7.jpg") {
		t.Errorf("long filename = %q, want page suffix", got)
	}
}
