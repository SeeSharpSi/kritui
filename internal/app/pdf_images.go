package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

const (
	maxConvertedPDFPages = 200
	maxConvertedPDFBytes = 80 << 20
	pdfRenderTimeout     = 2 * time.Minute
	pdfRenderDPI         = 100
	pdfRenderMaxSize     = 1600
)

var (
	errSelectedModelDoesNotSupportImages = errors.New("selected model does not support images")
	errModelDoesNotSupportPDFsOrImages   = errors.New("this model doesn't support PDFs or images")
)

// pdfConversionError reports a user-readable failure while rendering PDF pages
// for a model without native PDF input.
type pdfConversionError struct {
	message string
}

func (e *pdfConversionError) Error() string { return e.message }

func newPDFConversionError(format string, values ...any) error {
	return &pdfConversionError{message: fmt.Sprintf(format, values...)}
}

// modelSupportWithClient returns metadata for one model. The boolean is false
// when the provider did not list the model or metadata could not be fetched.
func modelSupportWithClient(ctx context.Context, client *llm.Client, model string) (llm.ModelInfo, bool) {
	infos, err := client.ModelInfos(ctx)
	if err != nil {
		return llm.ModelInfo{}, false
	}
	for _, info := range infos {
		if info.ID == model {
			return info, true
		}
	}
	return llm.ModelInfo{}, false
}

func modelSupport(ctx context.Context, config kritui_db.LLMConfig, model, sessionID string) (llm.ModelInfo, bool) {
	client, err := llm.New(config.APIKey, model, config.Endpoint, llm.ClientOptions{SessionID: sessionID})
	if err != nil {
		return llm.ModelInfo{}, false
	}
	return modelSupportWithClient(ctx, client, model)
}

// prepareMessagesForModel replaces PDF attachments with page images when the
// model cannot read PDFs directly. Converted messages receive the built-in PDF
// image prompt append. Original messages are never modified.
func prepareMessagesForModel(ctx context.Context, messages []llm.Message, support llm.ModelInfo) ([]llm.Message, error) {
	if messagesHaveImageAttachments(messages) && support.ImageSupport == llm.InputSupportUnsupported {
		return nil, errSelectedModelDoesNotSupportImages
	}
	if !messagesHavePDFAttachments(messages) || support.PDFSupport != llm.InputSupportUnsupported {
		return messages, nil
	}
	if support.ImageSupport == llm.InputSupportUnsupported {
		return nil, errModelDoesNotSupportPDFsOrImages
	}

	appendText := kritui_db.PDFImagesAppendText()

	converted := cloneMessagesForPDFConversion(messages)
	for index := range converted {
		if converted[index].Role != "user" {
			continue
		}
		replacements := make([]llm.UserAttachment, 0, len(converted[index].Attachments))
		didConvert := false
		for _, attachment := range converted[index].Attachments {
			if attachment.MediaType != "application/pdf" {
				replacements = append(replacements, attachment)
				continue
			}
			pages, err := renderPDFPages(ctx, attachment)
			if err != nil {
				return nil, err
			}
			replacements = append(replacements, pages...)
			didConvert = true
		}
		if !didConvert {
			continue
		}
		converted[index].Attachments = replacements
		addPromptAppendText(&converted[index], appendText)
	}
	return converted, nil
}

func cloneMessagesForPDFConversion(messages []llm.Message) []llm.Message {
	cloned := make([]llm.Message, len(messages))
	copy(cloned, messages)
	for index := range cloned {
		cloned[index].Attachments = append([]llm.UserAttachment(nil), messages[index].Attachments...)
	}
	return cloned
}

func addPromptAppendText(message *llm.Message, text string) {
	message.PromptAppendTexts = appendPromptAppendText(message.PromptAppendTexts, text)
}

func appendPromptAppendText(texts []string, text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return texts
	}
	for _, existing := range texts {
		if existing == text {
			return texts
		}
	}
	return append(texts, text)
}

func messagesHavePDFAttachments(messages []llm.Message) bool {
	for _, message := range messages {
		for _, attachment := range message.Attachments {
			if attachment.MediaType == "application/pdf" {
				return true
			}
		}
	}
	return false
}

func hasPDFAttachments(attachments []llm.UserAttachment) bool {
	for _, attachment := range attachments {
		if attachment.MediaType == "application/pdf" {
			return true
		}
	}
	return false
}

func renderPDFPages(ctx context.Context, attachment llm.UserAttachment) ([]llm.UserAttachment, error) {
	if attachment.MediaType != "application/pdf" || len(attachment.Data) == 0 {
		return nil, newPDFConversionError("The attached PDF could not be read for image conversion.")
	}

	renderContext, cancel := context.WithTimeout(ctx, pdfRenderTimeout)
	defer cancel()

	directory, err := os.MkdirTemp("", "kritui-pdf-")
	if err != nil {
		return nil, newPDFConversionError("The server could not create a temporary directory for PDF conversion.")
	}
	defer os.RemoveAll(directory)

	pdfPath := filepath.Join(directory, "input.pdf")
	if err := os.WriteFile(pdfPath, attachment.Data, 0o600); err != nil {
		return nil, newPDFConversionError("The server could not store the PDF for image conversion.")
	}

	pages, err := pdfPageCount(renderContext, pdfPath)
	if err != nil {
		return nil, err
	}
	if pages > maxConvertedPDFPages {
		return nil, newPDFConversionError("The PDF has %d pages; only PDFs with up to %d pages can be converted to images.", pages, maxConvertedPDFPages)
	}

	prefix := filepath.Join(directory, "page")
	args := []string{
		"-jpeg",
		"-r", strconv.Itoa(pdfRenderDPI),
		"-scale-to", strconv.Itoa(pdfRenderMaxSize),
		"-jpegopt", "quality=80",
		"-f", "1",
		"-l", strconv.Itoa(pages),
		pdfPath,
		prefix,
	}
	if err := runPDFCommand(renderContext, "pdftoppm", args...); err != nil {
		return nil, err
	}

	rendered, err := renderedPDFPageFiles(directory)
	if err != nil {
		return nil, err
	}
	if len(rendered) != pages {
		return nil, newPDFConversionError("The PDF could not be converted to one image per page.")
	}

	attachments := make([]llm.UserAttachment, 0, len(rendered))
	var total int64
	for _, page := range rendered {
		total += page.size
		if total > maxConvertedPDFBytes {
			return nil, newPDFConversionError("The PDF is too large to convert to images.")
		}
		data, err := os.ReadFile(page.path)
		if err != nil {
			return nil, newPDFConversionError("The server could not read a converted PDF page.")
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return nil, newPDFConversionError("The PDF produced an invalid page image.")
		}
		attachments = append(attachments, llm.UserAttachment{
			Filename:  pdfPageFilename(attachment.Filename, page.number),
			MediaType: "image/jpeg",
			Width:     config.Width,
			Height:    config.Height,
			Data:      data,
		})
	}
	return attachments, nil
}

func pdfPageCount(ctx context.Context, pdfPath string) (int, error) {
	output, err := runPDFCommandOutput(ctx, "pdfinfo", pdfPath)
	if err != nil {
		return 0, err
	}
	match := regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`).FindSubmatch(output)
	if match == nil {
		return 0, newPDFConversionError("The PDF page count could not be determined.")
	}
	pages, err := strconv.Atoi(string(match[1]))
	if err != nil || pages < 1 {
		return 0, newPDFConversionError("The PDF contains no readable pages.")
	}
	return pages, nil
}

type renderedPDFPage struct {
	number int
	path   string
	size   int64
}

func renderedPDFPageFiles(directory string) ([]renderedPDFPage, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, newPDFConversionError("The server could not inspect converted PDF pages.")
	}
	pattern := regexp.MustCompile(`^page-(\d+)\.jpg$`)
	pages := make([]renderedPDFPage, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number < 1 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, newPDFConversionError("The server could not inspect a converted PDF page.")
		}
		pages = append(pages, renderedPDFPage{
			number: number,
			path:   filepath.Join(directory, entry.Name()),
			size:   info.Size(),
		})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].number < pages[j].number })
	if len(pages) == 0 {
		return nil, newPDFConversionError("The PDF produced no page images.")
	}
	return pages, nil
}

func pdfPageFilename(pdfName string, page int) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(pdfName, `\`, "/")))
	if name == "" || name == "." {
		name = "Attached PDF"
	}
	suffix := fmt.Sprintf(" - page %d.jpg", page)
	runes := []rune(name)
	limit := 255 - len([]rune(suffix))
	if limit < 1 {
		limit = 1
	}
	if len(runes) > limit {
		name = string(runes[:limit])
	}
	return name + suffix
}

func runPDFCommand(ctx context.Context, name string, args ...string) error {
	_, err := runPDFCommandOutput(ctx, name, args...)
	return err
}

func runPDFCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, newPDFConversionError("PDF page rendering is unavailable because %s is not installed.", name)
	}
	command := exec.CommandContext(ctx, path, args...)
	command.WaitDelay = 5 * time.Second
	var stderr limitedWriter
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, newPDFConversionError("PDF image conversion timed out or was canceled.")
		}
		return nil, newPDFConversionError("The PDF could not be converted to images.")
	}
	return output, nil
}

type limitedWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.limit == 0 {
		w.limit = 8 << 10
	}
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = w.buffer.Write(data[:remaining])
	}
	return len(data), nil
}
