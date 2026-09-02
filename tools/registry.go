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

// Tool is one model-callable function. Execute must validate arguments against
// Definition.Parameters before performing side effects.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments json.RawMessage) (string, error)
}

// CapabilityTool optionally groups several tools under one selectable
// capability name. Ordinary tools expose their Definition.Name as a
// single-tool capability; multiple tools may share a capability only when
// every one of them implements CapabilityTool.
type CapabilityTool interface {
	Tool
	Capability() string
}

type registeredTool struct {
	definition Definition
	tool       Tool
	capability string
}

type capabilityGroup struct {
	explicitOnly bool
}

// Registry is an immutable set of tools and is safe for concurrent use. Tool
// implementations remain responsible for making their Execute methods safe for
// concurrent calls. Use Select to derive a least-privilege registry for a user
// or request.
type Registry struct {
	byName       map[string]registeredTool
	order        []string
	capabilities map[string]capabilityGroup
	capOrder     []string
}

// NewRegistry creates a registry and validates every tool definition. Tool
// order is preserved so outbound LLM requests remain deterministic.
func NewRegistry(toolList ...Tool) (*Registry, error) {
	registry := &Registry{
		byName:       make(map[string]registeredTool, len(toolList)),
		order:        make([]string, 0, len(toolList)),
		capabilities: make(map[string]capabilityGroup, len(toolList)),
		capOrder:     make([]string, 0, len(toolList)),
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
		capability := definition.Name
		grouped := false
		if capabilityTool, ok := tool.(CapabilityTool); ok {
			capability = capabilityTool.Capability()
			if err := validateName(capability, "capability"); err != nil {
				return nil, fmt.Errorf("tools: register %q: %w", definition.Name, err)
			}
			grouped = true
		}

		if group, exists := registry.capabilities[capability]; exists {
			if !grouped || !group.explicitOnly {
				return nil, fmt.Errorf("tools: register %q: capability %q may be shared only by CapabilityTool implementations", definition.Name, capability)
			}
		} else {
			registry.capabilities[capability] = capabilityGroup{
				explicitOnly: grouped,
			}
			registry.capOrder = append(registry.capOrder, capability)
		}

		registry.byName[definition.Name] = registeredTool{
			definition: definition,
			tool:       tool,
			capability: capability,
		}
		registry.order = append(registry.order, definition.Name)
	}

	return registry, nil
}

// Names returns selectable capability names in registration order. An ordinary
// tool contributes its own definition name, while CapabilityTool
// implementations contribute their shared capability name.
func (r *Registry) Names() []string {
	return slices.Clone(r.capOrder)
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

// With returns a new registry containing the receiver's tools followed by
// toolList. The receiver remains unchanged.
func (r *Registry) With(toolList ...Tool) (*Registry, error) {
	combined := make([]Tool, 0, len(r.order)+len(toolList))
	for _, name := range r.order {
		combined = append(combined, r.byName[name].tool)
	}
	combined = append(combined, toolList...)
	return NewRegistry(combined...)
}

// Lookup returns a registered tool by its executable definition name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	registered, ok := r.byName[name]
	return registered.tool, ok
}

// HasCapability reports whether name is a selectable capability in this
// registry.
func (r *Registry) HasCapability(name string) bool {
	_, ok := r.capabilities[name]
	return ok
}

// Select returns a registry containing only tools whose capabilities were
// named. Unknown capability names return an error rather than silently
// broadening or partially applying access. Capability and definition order are
// retained, repeated names are ignored, and every executable tool sharing a
// selected capability is included.
func (r *Registry) Select(capabilities ...string) (*Registry, error) {
	selectedCapabilities := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if _, exists := r.capabilities[capability]; !exists {
			return nil, fmt.Errorf("%w: %q", ErrToolNotFound, capability)
		}
		selectedCapabilities[capability] = struct{}{}
	}

	selected := &Registry{
		byName:       make(map[string]registeredTool, len(selectedCapabilities)),
		order:        make([]string, 0, len(selectedCapabilities)),
		capabilities: make(map[string]capabilityGroup, len(selectedCapabilities)),
		capOrder:     make([]string, 0, len(selectedCapabilities)),
	}
	for _, capability := range r.capOrder {
		if _, enabled := selectedCapabilities[capability]; !enabled {
			continue
		}
		selected.capabilities[capability] = r.capabilities[capability]
		selected.capOrder = append(selected.capOrder, capability)
	}
	for _, name := range r.order {
		if _, enabled := selectedCapabilities[r.byName[name].capability]; !enabled {
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

func validateName(name, kind string) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(name) > maxToolNameLength {
		return fmt.Errorf("%s name %q exceeds %d characters", kind, name, maxToolNameLength)
	}
	if !toolNamePattern.MatchString(name) {
		return fmt.Errorf("%s name %q may contain only letters, digits, underscores, and hyphens", kind, name)
	}
	return nil
}

func validateDefinition(definition Definition) error {
	if err := validateName(definition.Name, "tool"); err != nil {
		return err
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
