package llm

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

//go:embed system_prompt.md
var systemPrompt string

// PromptContext supplies request-specific context appended to the system prompt.
// A nil ClientLocation means the client's timezone is unavailable.
type PromptContext struct {
	CurrentTime    time.Time
	ClientLocation *time.Location
}

func systemPromptWithContext(promptContext PromptContext) string {
	currentUTC := promptContext.CurrentTime.UTC()
	prompt := fmt.Sprintf(
		"%s\n\n## Current date and time\nCurrent UTC datetime: %s",
		strings.TrimSpace(systemPrompt),
		currentUTC.Format(time.RFC3339),
	)
	if promptContext.ClientLocation == nil {
		return prompt + "\nClient may be in different timezone; if giving times, specify that they're in UTC."
	}
	return fmt.Sprintf(
		"%s\nClient datetime: %s\nClient timezone: %s",
		prompt,
		currentUTC.In(promptContext.ClientLocation).Format(time.RFC3339),
		promptContext.ClientLocation.String(),
	)
}
