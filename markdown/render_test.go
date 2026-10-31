package markdown

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	content := "# Answer\n\n- first\n- second\n\n**bold** and `code`\nnext line"
	var output bytes.Buffer

	if err := Render(content).Render(context.Background(), &output); err != nil {
		t.Fatalf("render Markdown: %v", err)
	}

	rendered := output.String()
	for _, want := range []string{
		"<h1>Answer</h1>",
		"<li>first</li>",
		"<li>second</li>",
		"<strong>bold</strong>",
		"<code>code</code>",
		"<br>",
		"next line",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered Markdown does not contain %q: %s", want, rendered)
		}
	}
}

func TestRenderSanitizesHTML(t *testing.T) {
	content := "[unsafe](javascript:alert(1))\n\n<script>alert('bad')</script>"
	var output bytes.Buffer

	if err := Render(content).Render(context.Background(), &output); err != nil {
		t.Fatalf("render Markdown: %v", err)
	}

	rendered := output.String()
	for _, unsafe := range []string{"javascript:", "<script", "alert('bad')"} {
		if strings.Contains(rendered, unsafe) {
			t.Errorf("rendered Markdown contains unsafe content %q: %s", unsafe, rendered)
		}
	}
}

func TestRenderOpensAttachmentCitationLinksInNewTab(t *testing.T) {
	content := "[page 3](/chats/1/messages/2/attachments/0#page=3)\n\n[page 4](http://localhost:8080/chats/1/messages/2/attachments/1#page=4)\n\n[relative](notes.pdf)"
	var output bytes.Buffer

	if err := Render(content).Render(context.Background(), &output); err != nil {
		t.Fatalf("render Markdown: %v", err)
	}

	rendered := output.String()
	for _, want := range []string{
		`href="/chats/1/messages/2/attachments/0#page=3"`,
		`href="http://localhost:8080/chats/1/messages/2/attachments/1#page=4"`,
		`href="notes.pdf"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered Markdown does not contain %q: %s", want, rendered)
		}
	}
	if got, want := strings.Count(rendered, `target="_blank"`), 2; got != want {
		t.Errorf("rendered Markdown has %d new-tab links, want %d: %s", got, want, rendered)
	}
	if !strings.Contains(rendered, `rel="nofollow noopener"`) {
		t.Errorf("rendered attachment links lack rel=noopener: %s", rendered)
	}
	notesIndex := strings.Index(rendered, `href="notes.pdf"`)
	if notesIndex < 0 || strings.Contains(rendered[notesIndex:], `target="_blank"`) {
		t.Errorf("rendered relative non-attachment link was forced to open in a new tab: %s", rendered)
	}
}

func TestRenderOpensExternalLinksInNewTab(t *testing.T) {
	var output bytes.Buffer

	if err := Render("[example](https://example.com)").Render(context.Background(), &output); err != nil {
		t.Fatalf("render Markdown: %v", err)
	}

	rendered := output.String()
	for _, want := range []string{`target="_blank"`, `rel="nofollow noopener"`} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered Markdown does not contain %q: %s", want, rendered)
		}
	}
}
