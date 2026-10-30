package llm

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

//go:embed system_prompt.md
var systemPrompt string

// invariantPDFInstructions is appended after the editable system prompt base.
// It keeps PDF citation guidance available even when an account customizes its
// own instructions.
const invariantPDFInstructions = "When the user attaches PDFs, cite pages with Markdown links to the exact attachment URL supplied in the \"PDF attachments\" section of your system context. Append `#page=N` to that URL when you know the page, where N is the 1-based physical page index counted from the start of the PDF, not the printed page label. Give the link a descriptive label such as the filename and page. Do not invent attachment URLs or page numbers. If the page is unknown, link to the PDF without a page fragment."

// DefaultSystemPrompt returns the built-in editable system prompt base without
// surrounding whitespace. Accounts that have not customized their prompt use
// this value.
func DefaultSystemPrompt() string {
	return strings.TrimSpace(systemPrompt)
}

// PDFLink identifies one PDF attachment in the active conversation so the model
// can cite it with a stable link. Filename is untrusted display data; URL is the
// exact root-relative attachment endpoint for that file.
type PDFLink struct {
	Filename string
	URL      string
}

// PromptContext supplies request-specific context appended to the system prompt.
// An empty or whitespace SystemPrompt selects the built-in default base. A nil
// ClientLocation means the client's timezone is unavailable.
type PromptContext struct {
	SystemPrompt   string
	CurrentTime    time.Time
	ClientLocation *time.Location
	PDFLinks       []PDFLink
}

// systemPromptBase returns the account's custom base byte-for-byte, or the
// built-in default when no custom text is supplied. Only blank and whitespace
// values select the default; intentional leading and trailing whitespace in a
// custom prompt is preserved exactly.
func systemPromptBase(promptContext PromptContext) string {
	if strings.TrimSpace(promptContext.SystemPrompt) == "" {
		return DefaultSystemPrompt()
	}
	return promptContext.SystemPrompt
}

func systemPromptWithContext(promptContext PromptContext) string {
	currentUTC := promptContext.CurrentTime.UTC()
	prompt := fmt.Sprintf(
		"%s\n\n%s\n\n## Current date and time\nCurrent UTC datetime: %s",
		systemPromptBase(promptContext),
		invariantPDFInstructions,
		currentUTC.Format(time.RFC3339),
	)
	if promptContext.ClientLocation == nil {
		prompt += "\nClient may be in different timezone; if giving times, specify that they're in UTC."
	} else {
		prompt = fmt.Sprintf(
			"%s\nClient datetime: %s\nClient timezone: %s",
			prompt,
			currentUTC.In(promptContext.ClientLocation).Format(time.RFC3339),
			promptContext.ClientLocation.String(),
		)
	}
	if len(promptContext.PDFLinks) == 0 {
		return prompt
	}

	var builder strings.Builder
	builder.WriteString(prompt)
	builder.WriteString("\n\n## PDF attachments")
	for _, link := range promptContext.PDFLinks {
		filename := strings.TrimSpace(link.Filename)
		if filename == "" {
			filename = "Attached PDF"
		}
		fmt.Fprintf(&builder, "\n- Filename: %q; URL: %s", filename, link.URL)
	}
	return builder.String()
}
