package markdown

import (
	"bytes"
	"net/url"
	"regexp"

	"github.com/a-h/templ"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var attachmentPathPattern = regexp.MustCompile(`^/chats/[1-9]\d*/messages/[1-9]\d*/attachments/\d+$`)

type attachmentLinkTransformer struct{}

// Transform opens Markdown links to message attachments in a new tab.
func (attachmentLinkTransformer) Transform(node *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(node, func(current ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := current.(*ast.Link)
		if !ok || !isAttachmentURL(link.Destination) {
			return ast.WalkContinue, nil
		}
		link.SetAttributeString("target", "_blank")
		return ast.WalkContinue, nil
	})
}

func isAttachmentURL(destination []byte) bool {
	parsed, err := url.Parse(string(destination))
	if err != nil {
		return false
	}
	return attachmentPathPattern.MatchString(parsed.Path)
}

var renderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithHardWraps()),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(attachmentLinkTransformer{}, 100))),
)

var policy = func() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()
	policy.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[A-Za-z0-9_+-]+$`)).OnElements("code")
	policy.AllowAttrs("target").OnElements("a")
	policy.AddTargetBlankToFullyQualifiedLinks(true)
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
