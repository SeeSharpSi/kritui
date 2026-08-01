// Package tools defines LLM-callable tools and their registry.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

const maxToolNameLength = 64

var (
	// ErrToolNotFound indicates that a tool is unavailable in a registry.
	ErrToolNotFound = errors.New("tools: tool not found")

	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Definition describes a tool to an LLM. Parameters must contain a JSON
// Schema whose root is an object.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Tool is one capability exposed to an LLM. Execute must validate arguments
// against Definition.Parameters before performing side effects.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments json.RawMessage) (string, error)
}

type registeredTool struct {
	definition Definition
	tool       Tool
}

// Registry is an immutable set of tools and is safe for concurrent use. Tool
// implementations remain responsible for making their Execute methods safe for
// concurrent calls. Use Select to derive a least-privilege registry for a user
// or request.
type Registry struct {
	byName map[string]registeredTool
	order  []string
}

// NewRegistry creates a registry and validates every tool definition. Tool
// order is preserved so outbound LLM requests remain deterministic.
func NewRegistry(toolList ...Tool) (*Registry, error) {
	registry := &Registry{
		byName: make(map[string]registeredTool, len(toolList)),
		order:  make([]string, 0, len(toolList)),
	}

	for index, tool := range toolList {
		if isNilTool(tool) {
			return nil, fmt.Errorf("tools: register tool at index %d: tool is nil", index)
		}

		definition := tool.Definition()
		if err := validateDefinition(definition); err != nil {
			return nil, fmt.Errorf("tools: register tool at index %d: %w", index, err)
		}
		if _, exists := registry.byName[definition.Name]; exists {
			return nil, fmt.Errorf("tools: register %q: duplicate tool name", definition.Name)
		}

		definition = cloneDefinition(definition)
		registry.byName[definition.Name] = registeredTool{
			definition: definition,
			tool:       tool,
		}
		registry.order = append(registry.order, definition.Name)
	}

	return registry, nil
}

// Names returns registered tool names in registration order.
func (r *Registry) Names() []string {
	return slices.Clone(r.order)
}

// Definitions returns tool definitions in registration order. Returned values
// may be modified without changing the registry.
func (r *Registry) Definitions() []Definition {
	definitions := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definitions = append(definitions, cloneDefinition(r.byName[name].definition))
	}
	return definitions
}

// Lookup returns a registered tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	registered, ok := r.byName[name]
	return registered.tool, ok
}

// Select returns a registry containing only named tools. Unknown names return
// an error rather than silently broadening or partially applying access.
// Registry order is retained, and repeated names are ignored.
func (r *Registry) Select(names ...string) (*Registry, error) {
	selectedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := r.byName[name]; !exists {
			return nil, fmt.Errorf("%w: %q", ErrToolNotFound, name)
		}
		selectedNames[name] = struct{}{}
	}

	selected := &Registry{
		byName: make(map[string]registeredTool, len(selectedNames)),
		order:  make([]string, 0, len(selectedNames)),
	}
	for _, name := range r.order {
		if _, enabled := selectedNames[name]; !enabled {
			continue
		}
		selected.byName[name] = r.byName[name]
		selected.order = append(selected.order, name)
	}

	return selected, nil
}

// Execute invokes a tool from this registry. A tool omitted by Select cannot
// be invoked through the selected registry.
func (r *Registry) Execute(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	registered, exists := r.byName[name]
	if !exists {
		return "", fmt.Errorf("%w: %q", ErrToolNotFound, name)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return "", fmt.Errorf("tools: execute %q: arguments must be a JSON object", name)
	}

	return registered.tool.Execute(ctx, arguments)
}

func validateDefinition(definition Definition) error {
	if definition.Name == "" {
		return errors.New("tool name is required")
	}
	if len(definition.Name) > maxToolNameLength {
		return fmt.Errorf("tool name %q exceeds %d characters", definition.Name, maxToolNameLength)
	}
	if !toolNamePattern.MatchString(definition.Name) {
		return fmt.Errorf("tool name %q may contain only letters, digits, underscores, and hyphens", definition.Name)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return fmt.Errorf("tool %q description is required", definition.Name)
	}

	var schema map[string]json.RawMessage
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil || schema == nil {
		return fmt.Errorf("tool %q parameters must be a JSON object schema", definition.Name)
	}
	var schemaType string
	if err := json.Unmarshal(schema["type"], &schemaType); err != nil || schemaType != "object" {
		return fmt.Errorf("tool %q parameters schema type must be object", definition.Name)
	}
	return nil
}

func cloneDefinition(definition Definition) Definition {
	definition.Parameters = append(json.RawMessage(nil), definition.Parameters...)
	return definition
}

func isNilTool(tool Tool) bool {
	if tool == nil {
		return true
	}

	value := reflect.ValueOf(tool)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
