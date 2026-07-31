package markdown

import (
	"bytes"
	"regexp"

	"github.com/a-h/templ"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var renderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

var policy = func() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()
	policy.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[A-Za-z0-9_+-]+$`)).OnElements("code")
	return policy
}()

// Render converts untrusted Markdown into sanitized HTML.
func Render(content string) templ.Component {
	var rendered bytes.Buffer
	if err := renderer.Convert([]byte(content), &rendered); err != nil {
		return templ.Raw("", err)
	}

	return templ.Raw(string(policy.SanitizeBytes(rendered.Bytes())))
}
