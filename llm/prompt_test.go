package llm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSystemPromptWithPDFLinks(t *testing.T) {
	currentTime := time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)
	clientLocation := time.FixedZone("America/New_York", -4*60*60)
	links := []PDFLink{
		{Filename: "same-report.pdf", URL: "/chats/12/messages/34/attachments/1"},
		{Filename: "same-report.pdf", URL: "/chats/12/messages/56/attachments/0"},
	}

	tests := []struct {
		name       string
		location   *time.Location
		lastDetail string
	}{
		{
			name:       "client timezone",
			location:   clientLocation,
			lastDetail: "Client timezone: America/New_York",
		},
		{
			name:       "no client timezone",
			lastDetail: "Client may be in different timezone; if giving times, specify that they're in UTC.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prompt := systemPromptWithContext(PromptContext{
				CurrentTime:    currentTime,
				ClientLocation: test.location,
				PDFLinks:       links,
			})

			detailIndex := strings.Index(prompt, test.lastDetail)
			sectionIndex := strings.Index(prompt, "\n\n## PDF attachments")
			if detailIndex < 0 {
				t.Fatalf("prompt does not contain %q: %q", test.lastDetail, prompt)
			}
			if sectionIndex < 0 {
				t.Fatalf("prompt does not contain PDF metadata: %q", prompt)
			}
			if detailIndex > sectionIndex {
				t.Errorf("PDF metadata appears before timezone details: %q", prompt)
			}
			for _, link := range links {
				want := fmt.Sprintf(`Filename: %q; URL: %s`, link.Filename, link.URL)
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt does not contain %q: %q", want, prompt)
				}
			}
			if count := strings.Count(prompt, "\n- Filename:"); count != len(links) {
				t.Errorf("PDF metadata entries = %d, want %d: %q", count, len(links), prompt)
			}
		})
	}
}

func TestSystemPromptWithoutPDFLinksIsUnchanged(t *testing.T) {
	currentTime := time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)
	clientLocation := time.FixedZone("America/New_York", -4*60*60)

	tests := []struct {
		name     string
		location *time.Location
		suffix   string
	}{
		{
			name:     "client timezone",
			location: clientLocation,
			suffix:   "Client timezone: America/New_York",
		},
		{
			name:   "no client timezone",
			suffix: "Client may be in different timezone; if giving times, specify that they're in UTC.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prompt := systemPromptWithContext(PromptContext{
				CurrentTime:    currentTime,
				ClientLocation: test.location,
			})
			if strings.Contains(prompt, "## PDF attachments") {
				t.Errorf("prompt without PDFs contains PDF metadata: %q", prompt)
			}
			if !strings.HasSuffix(prompt, test.suffix) {
				t.Errorf("prompt suffix = %q, want %q", prompt, test.suffix)
			}
		})
	}
}

func TestDefaultSystemPromptIsEditableBase(t *testing.T) {
	got := DefaultSystemPrompt()
	if got != "Your name is bob" {
		t.Errorf("DefaultSystemPrompt() = %q, want built-in base", got)
	}
	if strings.Contains(got, "PDF") {
		t.Errorf("DefaultSystemPrompt() includes invariant PDF guidance: %q", got)
	}
}

func TestSystemPromptComposesCustomBaseInvariantAndRuntimeContext(t *testing.T) {
	currentTime := time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)
	clientLocation := time.FixedZone("America/New_York", -4*60*60)
	custom := "You are Kritui.\nAnswer briefly."
	prompt := systemPromptWithContext(PromptContext{
		SystemPrompt:   custom,
		CurrentTime:    currentTime,
		ClientLocation: clientLocation,
		PDFLinks:       []PDFLink{{Filename: "report.pdf", URL: "/chats/1/messages/2/attachments/0"}},
	})

	wantPrefix := custom + "\n\n" + invariantPDFInstructions + "\n\n## Current date and time\nCurrent UTC datetime: " +
		currentTime.Format(time.RFC3339)
	if !strings.HasPrefix(prompt, wantPrefix) {
		t.Errorf("prompt = %q, want prefix %q", prompt, wantPrefix)
	}
	if count := strings.Count(prompt, invariantPDFInstructions); count != 1 {
		t.Errorf("invariant PDF guidance count = %d, want 1: %q", count, prompt)
	}
	if count := strings.Count(prompt, custom); count != 1 {
		t.Errorf("custom base count = %d, want 1: %q", count, prompt)
	}
	if !strings.Contains(prompt, "Client timezone: America/New_York") ||
		!strings.Contains(prompt, `Filename: "report.pdf"; URL: /chats/1/messages/2/attachments/0`) {
		t.Errorf("prompt lacks runtime context: %q", prompt)
	}
}

func TestSystemPromptPreservesCustomBaseWhitespaceExactly(t *testing.T) {
	currentTime := time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC)
	custom := "\n\n  Custom base keeps its spacing.  \n\nSecond paragraph.\n\n"
	prompt := systemPromptWithContext(PromptContext{
		SystemPrompt: custom,
		CurrentTime:  currentTime,
	})

	if !strings.HasPrefix(prompt, custom+"\n\n"+invariantPDFInstructions+"\n\n## Current date and time\n") {
		t.Errorf("prompt = %q, want raw custom prefix %q", prompt, custom+"\n\n"+invariantPDFInstructions)
	}
	if strings.HasPrefix(prompt, strings.TrimSpace(custom)) {
		t.Errorf("prompt starts with trimmed custom text: %q", prompt)
	}
	if strings.Contains(prompt, DefaultSystemPrompt()) {
		t.Errorf("prompt includes built-in base despite custom text: %q", prompt)
	}
	if count := strings.Count(prompt, custom); count != 1 {
		t.Errorf("raw custom base count = %d, want 1: %q", count, prompt)
	}
}

func TestSystemPromptWhitespaceUsesBuiltInBase(t *testing.T) {
	promptContext := PromptContext{
		SystemPrompt: " \t\n ",
		CurrentTime:  time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC),
	}
	want := systemPromptWithContext(PromptContext{CurrentTime: promptContext.CurrentTime})
	if got := systemPromptWithContext(promptContext); got != want {
		t.Errorf("whitespace custom prompt = %q, want built-in composition %q", got, want)
	}
}

func TestSystemPromptEscapesPDFFilenamesAsSingleLineData(t *testing.T) {
	filename := "line\nbreak\t\"quoted\".pdf"
	links := []PDFLink{
		{Filename: filename, URL: "/chats/7/messages/8/attachments/1"},
		{Filename: "  \t ", URL: "/chats/7/messages/9/attachments/0"},
	}
	prompt := systemPromptWithContext(PromptContext{
		CurrentTime: time.Date(2026, time.August, 2, 18, 30, 0, 0, time.UTC),
		PDFLinks:    links,
	})

	_, section, ok := strings.Cut(prompt, "\n\n## PDF attachments\n")
	if !ok {
		t.Fatalf("prompt does not contain PDF metadata: %q", prompt)
	}
	lines := strings.Split(section, "\n")
	wantLines := []string{
		fmt.Sprintf(`- Filename: %q; URL: %s`, filename, links[0].URL),
		fmt.Sprintf(`- Filename: "Attached PDF"; URL: %s`, links[1].URL),
	}
	for index, want := range wantLines {
		got := ""
		if index < len(lines) {
			got = lines[index]
		}
		if got != want {
			t.Errorf("metadata line %d = %q, want %q", index, got, want)
		}
	}
	if len(lines) != len(wantLines) {
		t.Errorf("metadata line count = %d, want %d: %q", len(lines), len(wantLines), section)
	}
	if !strings.Contains(section, `\n`) || !strings.Contains(section, `\t`) || !strings.Contains(section, `\"`) {
		t.Errorf("filename control characters were not escaped: %q", section)
	}
}
