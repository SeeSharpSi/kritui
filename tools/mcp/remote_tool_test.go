package mcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConvertInputSchema(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		want    string
		wantErr string
	}{
		{
			name:  "missing schema defaults to object",
			input: nil,
			want:  `{"type":"object"}`,
		},
		{
			name:  "object schema preserved",
			input: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			want:  `{"properties":{"a":{"type":"string"}},"type":"object"}`,
		},
		{
			name:  "missing type injects object",
			input: map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}},
			want:  `{"properties":{"a":{"type":"string"}},"type":"object"}`,
		},
		{
			name:    "non-object type rejected",
			input:   map[string]any{"type": "string"},
			wantErr: "input schema type must be object",
		},
		{
			name:    "array schema rejected",
			input:   []any{"not", "an", "object"},
			wantErr: "input schema must be a JSON object",
		},
		{
			name:    "string schema rejected",
			input:   "not a schema",
			wantErr: "input schema must be a JSON object",
		},
		{
			name:    "null schema rejected",
			input:   json.RawMessage("null"),
			wantErr: "input schema must be a JSON object",
		},
		{
			name:    "unencodable schema rejected",
			input:   make(chan int),
			wantErr: "input schema is not JSON encodable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := convertInputSchema(test.input)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("convertInputSchema(%v) error = %v, want containing %q", test.input, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertInputSchema(%v) unexpected error: %v", test.input, err)
			}
			if string(got) != test.want {
				t.Fatalf("convertInputSchema(%v) = %s, want %s", test.input, got, test.want)
			}
		})
	}
}

func TestModelFacingName(t *testing.T) {
	if got := modelFacingName("git", "status"); got != modelFacingName("git", "status") {
		t.Fatalf("modelFacingName is not deterministic: %q vs %q", got, modelFacingName("git", "status"))
	}

	registryPattern := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for _, remoteName := range []string{"status", "weird name with spaces", "café/über-tool!", string([]rune{0x4e2d, 0x6587}), strings.Repeat("a", 300), "-"} {
		got := modelFacingName("git", remoteName)
		if len(got) > maxToolNameLength {
			t.Errorf("modelFacingName(%q) = %q exceeds %d bytes", remoteName, got, maxToolNameLength)
		}
		if !registryPattern.MatchString(got) {
			t.Errorf("modelFacingName(%q) = %q is not a valid registry name", remoteName, got)
		}
		separatorIndex := strings.LastIndex(got, nameSeparator)
		if len(got[separatorIndex+len(nameSeparator):]) != nameHashSuffixLength {
			t.Errorf("modelFacingName(%q) = %q lacks an %d-character hash suffix", remoteName, got, nameHashSuffixLength)
		}
	}

	if first, second := modelFacingName("git", "a b"), modelFacingName("git", "a.b"); first == second {
		t.Fatalf("different invalid names collapsed: %q", first)
	}
	if first, second := modelFacingName("git", "status"), modelFacingName("other", "status"); first == second {
		t.Fatalf("same remote name under different capabilities collapsed: %q", first)
	}
	if got := modelFacingName("git", ""); !strings.HasPrefix(got, fallbackNameBase) {
		t.Fatalf("empty remote name should use fallback base, got %q", got)
	}
}

func TestDecodeArguments(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "empty arguments", input: "", want: `{}`},
		{
			name:  "integer precision preserved",
			input: `{"n":9007199254740993}`,
			want:  `{"n":9007199254740993}`,
		},
		{name: "object", input: `{"a":1,"b":[true,null]}`, want: `{"a":1,"b":[true,null]}`},
		{name: "array rejected", input: `[1,2]`, wantErr: "arguments must be a JSON object"},
		{name: "string rejected", input: `"x"`, wantErr: "arguments must be a JSON object"},
		{name: "invalid JSON", input: `{`, wantErr: "arguments are not valid JSON"},
		{name: "multiple values", input: `{}{}`, wantErr: "arguments must be a single JSON object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeArguments(json.RawMessage(test.input))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("decodeArguments(%s) error = %v, want containing %q", test.input, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeArguments(%s) unexpected error: %v", test.input, err)
			}
			encoded, marshalErr := json.Marshal(got)
			if marshalErr != nil {
				t.Fatalf("marshal decoded arguments: %v", marshalErr)
			}
			if string(encoded) != test.want {
				t.Fatalf("decodeArguments(%s) marshaled = %s, want %s", test.input, encoded, test.want)
			}
		})
	}
}

func TestFormatToolResult(t *testing.T) {
	t.Run("text content", func(t *testing.T) {
		got, err := formatToolResult(&mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "one"}, &mcpsdk.TextContent{Text: "two"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "one\ntwo" {
			t.Fatalf("formatToolResult = %q", got)
		}
	})

	t.Run("non-text content encoded as JSON", func(t *testing.T) {
		got, err := formatToolResult(&mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("non-text content is not JSON: %v (%s)", err, got)
		}
		if decoded["type"] != "image" || decoded["mimeType"] != "image/png" {
			t.Fatalf("unexpected non-text content: %s", got)
		}
	})

	t.Run("structured content encoded as JSON", func(t *testing.T) {
		got, err := formatToolResult(&mcpsdk.CallToolResult{
			StructuredContent: map[string]any{"sum": 7.5},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != `{"sum":7.5}` {
			t.Fatalf("formatToolResult = %q", got)
		}
	})

	t.Run("order preserved across block kinds", func(t *testing.T) {
		got, err := formatToolResult(&mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "first"},
				&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{9}},
			},
			StructuredContent: map[string]any{"last": true},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		textIndex := strings.Index(got, "first")
		imageIndex := strings.Index(got, `"type":"image"`)
		structuredIndex := strings.Index(got, `"last":true`)
		if textIndex < 0 || imageIndex < 0 || structuredIndex < 0 || !(textIndex < imageIndex && imageIndex < structuredIndex) {
			t.Fatalf("output order wrong: %q", got)
		}
	})

	t.Run("error result becomes error with text", func(t *testing.T) {
		_, err := formatToolResult(&mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}},
			IsError: true,
		})
		if err == nil || !strings.Contains(err.Error(), "MCP tool call failed") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("formatToolResult error = %v, want MCP failure containing boom", err)
		}
	})

	t.Run("error result without content uses fallback", func(t *testing.T) {
		_, err := formatToolResult(&mcpsdk.CallToolResult{IsError: true})
		if err == nil || err.Error() != "MCP tool call failed" {
			t.Fatalf("formatToolResult error = %v, want fallback message", err)
		}
	})

	t.Run("empty result is concise", func(t *testing.T) {
		got, err := formatToolResult(&mcpsdk.CallToolResult{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "(empty result)" {
			t.Fatalf("formatToolResult = %q, want concise empty representation", got)
		}
	})

	t.Run("oversized output rejected", func(t *testing.T) {
		_, err := formatToolResult(&mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("a", maxOutputBytes+1)}},
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("formatToolResult error = %v, want size limit error", err)
		}
	})
}

func TestValidateConfig(t *testing.T) {
	valid := ServerConfig{Name: "docs", URL: "https://example.com/mcp", Capability: "docs"}
	if err := validateConfig(valid); err != nil {
		t.Fatalf("validateConfig(valid) unexpected error: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{name: "missing name", mutate: func(c *ServerConfig) { c.Name = "" }, wantErr: "server name is required"},
		{name: "missing capability", mutate: func(c *ServerConfig) { c.Capability = "" }, wantErr: "capability is required"},
		{name: "invalid capability", mutate: func(c *ServerConfig) { c.Capability = "has spaces" }, wantErr: "capability"},
		{name: "overlong capability", mutate: func(c *ServerConfig) { c.Capability = strings.Repeat("a", maxToolNameLength+1) }, wantErr: "capability"},
		{name: "missing URL", mutate: func(c *ServerConfig) { c.URL = "" }, wantErr: "URL is required"},
		{name: "relative URL", mutate: func(c *ServerConfig) { c.URL = "/mcp" }, wantErr: "absolute"},
		{name: "non-http scheme", mutate: func(c *ServerConfig) { c.URL = "ftp://example.com/mcp" }, wantErr: "absolute"},
		{name: "missing host", mutate: func(c *ServerConfig) { c.URL = "http:///mcp" }, wantErr: "absolute"},
		{name: "unparsable URL", mutate: func(c *ServerConfig) { c.URL = "http://[::1" }, wantErr: "URL is invalid"},
		{name: "userinfo forbidden", mutate: func(c *ServerConfig) { c.URL = "https://user:pass@example.com/mcp" }, wantErr: "userinfo"},
		{name: "fragment forbidden", mutate: func(c *ServerConfig) { c.URL = "https://example.com/mcp#frag" }, wantErr: "fragment"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			err := validateConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateConfig(%+v) error = %v, want containing %q", config, err, test.wantErr)
			}
		})
	}
}
