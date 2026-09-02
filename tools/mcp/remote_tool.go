package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"seesharpsi/kritui/tools"
)

const (
	// callTimeout bounds a single remote tool execution unless the caller's
	// context expires first.
	callTimeout = 2 * time.Minute

	// maxOutputBytes caps the combined tool output at 1 MiB.
	maxOutputBytes = 1 << 20

	// nameHashSuffixLength is the number of hex characters kept from the
	// SHA-256 digest appended to every model-facing tool name.
	nameHashSuffixLength = 8

	// nameSeparator joins the sanitized name and hash suffix.
	nameSeparator = "-"

	// fallbackNameBase is used when a remote name sanitizes to nothing.
	fallbackNameBase = "tool"
)

// remoteTool adapts one remote MCP tool to the parent application's
// CapabilityTool interface. One server's tools all share the server's
// capability, making the server a single selectable capability.
type remoteTool struct {
	session    *mcpsdk.ClientSession
	config     ServerConfig
	remoteName string
	definition tools.Definition
}

func (t *remoteTool) Definition() tools.Definition {
	definition := t.definition
	definition.Parameters = append(json.RawMessage(nil), definition.Parameters...)
	return definition
}

func (t *remoteTool) Capability() string {
	return t.config.Capability
}

func (t *remoteTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	args, err := decodeArguments(arguments)
	if err != nil {
		return "", fmt.Errorf("mcp: execute %q: %w", t.definition.Name, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	result, err := t.session.CallTool(callCtx, &mcpsdk.CallToolParams{
		Name:      t.remoteName,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("mcp: execute %q: call failed: %w", t.definition.Name, err)
	}

	output, err := formatToolResult(result)
	if err != nil {
		return "", fmt.Errorf("mcp: execute %q: %w", t.definition.Name, err)
	}
	return output, nil
}

func newRemoteTool(sdkSession *mcpsdk.ClientSession, config ServerConfig, remote *mcpsdk.Tool) (*remoteTool, error) {
	if remote == nil {
		return nil, fmt.Errorf("mcp: server %q: empty tool listing entry", config.Name)
	}
	if remote.Name == "" {
		return nil, fmt.Errorf("mcp: server %q: remote tool has an empty name", config.Name)
	}
	parameters, err := convertInputSchema(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: server %q: remote tool %q: %w", config.Name, remote.Name, err)
	}

	description := strings.TrimSpace(remote.Description)
	if description == "" {
		description = fmt.Sprintf("Remote tool %q from MCP server %q.", remote.Name, config.Name)
	}

	return &remoteTool{
		session:    sdkSession,
		config:     config,
		remoteName: remote.Name,
		definition: tools.Definition{
			Name:        modelFacingName(config.Capability, remote.Name),
			Description: description,
			Parameters:  parameters,
		},
	}, nil
}

var remoteNameDisallowedPattern = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// modelFacingName builds a deterministic, collision-resistant registry name
// from the capability and the original remote tool name. The SHA-256 suffix is
// always included so different invalid names cannot collapse into the same
// sanitized prefix.
func modelFacingName(capability, remoteName string) string {
	digest := sha256.Sum256([]byte(capability + "\x00" + remoteName))
	suffix := hex.EncodeToString(digest[:])[:nameHashSuffixLength]

	base := remoteNameDisallowedPattern.ReplaceAllString(remoteName, "_")
	if base == "" {
		base = fallbackNameBase
	}
	// Sanitization is ASCII-only, so byte length equals rune length.
	if budget := maxToolNameLength - len(nameSeparator) - len(suffix); len(base) > budget {
		base = base[:budget]
	}
	return base + nameSeparator + suffix
}

// convertInputSchema validates the remote input schema and returns JSON whose
// root is an object with "type": "object", as required by the parent
// registry. A missing schema defaults to {"type":"object"}; malformed or
// non-object schemas are rejected instead of silently broadened.
func convertInputSchema(inputSchema any) (json.RawMessage, error) {
	if inputSchema == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	raw, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("input schema is not JSON encodable: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, errors.New("input schema must be a JSON object")
	}
	if rawType, exists := root["type"]; exists {
		var schemaType string
		if err := json.Unmarshal(rawType, &schemaType); err != nil || schemaType != "object" {
			return nil, errors.New("input schema type must be object")
		}
	} else {
		root["type"] = json.RawMessage(`"object"`)
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("input schema is not JSON encodable: %w", err)
	}
	return encoded, nil
}

// decodeArguments parses raw execution arguments into a JSON object while
// preserving integer precision through json.Number.
func decodeArguments(arguments json.RawMessage) (map[string]any, error) {
	if len(arguments) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("arguments must be a single JSON object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}

// formatToolResult renders a CallToolResult for the model. Text blocks are
// joined in order; non-text blocks and structured content are JSON-encoded so
// no data is silently dropped. Error results become Go errors carrying the
// safe returned text; output larger than maxOutputBytes is rejected.
func formatToolResult(result *mcpsdk.CallToolResult) (string, error) {
	if result == nil {
		return "", errors.New("MCP tool call failed")
	}

	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcpsdk.TextContent); ok {
			parts = append(parts, text.Text)
			continue
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			encoded, _ = json.Marshal(map[string]string{
				"type":    "unencodable",
				"content": fmt.Sprintf("%T", content),
			})
		}
		parts = append(parts, string(encoded))
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("structured content is not JSON encodable: %w", err)
		}
		parts = append(parts, string(encoded))
	}

	combined := strings.Join(parts, "\n")
	if len(combined) > maxOutputBytes {
		return "", fmt.Errorf("tool result exceeds %d bytes", maxOutputBytes)
	}

	if result.IsError {
		if strings.TrimSpace(combined) == "" {
			return "", errors.New("MCP tool call failed")
		}
		return "", fmt.Errorf("MCP tool call failed: %s", combined)
	}

	if len(parts) == 0 {
		return "(empty result)", nil
	}
	return combined, nil
}
