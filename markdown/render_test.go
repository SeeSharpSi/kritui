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
