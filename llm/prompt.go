package llm

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

//go:embed system_prompt.md
var systemPrompt string

// PDFLink identifies one PDF attachment in the active conversation so the model
// can cite it with a stable link. Filename is untrusted display data; URL is the
// exact root-relative attachment endpoint for that file.
type PDFLink struct {
	Filename string
	URL      string
}

// PromptContext supplies request-specific context appended to the system prompt.
// A nil ClientLocation means the client's timezone is unavailable.
type PromptContext struct {
	CurrentTime    time.Time
	ClientLocation *time.Location
	PDFLinks       []PDFLink
}

func systemPromptWithContext(promptContext PromptContext) string {
	currentUTC := promptContext.CurrentTime.UTC()
	prompt := fmt.Sprintf(
		"%s\n\n## Current date and time\nCurrent UTC datetime: %s",
		strings.TrimSpace(systemPrompt),
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
