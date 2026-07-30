package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type stubTool struct {
	definition Definition
	execute    func(context.Context, json.RawMessage) (string, error)
}

func (t stubTool) Definition() Definition {
	return t.definition
}

func (t stubTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	if t.execute == nil {
		return "", nil
	}
	return t.execute(ctx, arguments)
}

func newStubTool(name string) stubTool {
	return stubTool{
		definition: Definition{
			Name:        name,
			Description: "Use " + name + ".",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		},
	}
}

func TestNewRegistryPreservesOrderAndCopiesDefinitions(t *testing.T) {
	first := newStubTool("first")
	second := newStubTool("second")
	originalSchema := append(json.RawMessage(nil), first.definition.Parameters...)

	registry, err := NewRegistry(first, second)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	first.definition.Parameters[0] = '['
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("Names() = %v, want [first second]", got)
	}

	definitions := registry.Definitions()
	if !reflect.DeepEqual(definitions[0].Parameters, originalSchema) {
		t.Fatalf("Definitions()[0].Parameters = %s, want %s", definitions[0].Parameters, originalSchema)
	}
	definitions[0].Parameters[0] = '['
	if got := registry.Definitions()[0].Parameters; !reflect.DeepEqual(got, originalSchema) {
		t.Fatalf("registry schema changed through returned definition: %s", got)
	}
}

func TestNewRegistryRejectsInvalidTools(t *testing.T) {
	valid := newStubTool("valid")
	var nilTool *stubTool

	tests := []struct {
		name  string
		tools []Tool
		want  string
	}{
		{name: "nil", tools: []Tool{nil}, want: "tool is nil"},
		{name: "typed nil", tools: []Tool{nilTool}, want: "tool is nil"},
		{name: "missing name", tools: []Tool{stubTool{definition: Definition{Description: "description", Parameters: json.RawMessage(`{}`)}}}, want: "name is required"},
		{name: "invalid name", tools: []Tool{stubTool{definition: Definition{Name: "bad name", Description: "description", Parameters: json.RawMessage(`{}`)}}}, want: "may contain only"},
		{name: "long name", tools: []Tool{stubTool{definition: Definition{Name: strings.Repeat("a", 65), Description: "description", Parameters: json.RawMessage(`{}`)}}}, want: "exceeds 64"},
		{name: "missing description", tools: []Tool{stubTool{definition: Definition{Name: "name", Parameters: json.RawMessage(`{}`)}}}, want: "description is required"},
		{name: "invalid schema", tools: []Tool{stubTool{definition: Definition{Name: "name", Description: "description", Parameters: json.RawMessage(`[]`)}}}, want: "JSON object schema"},
		{name: "non-object schema type", tools: []Tool{stubTool{definition: Definition{Name: "name", Description: "description", Parameters: json.RawMessage(`{"type":"array"}`)}}}, want: "schema type must be object"},
		{name: "duplicate", tools: []Tool{valid, valid}, want: "duplicate tool name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.tools...); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewRegistry() error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestSelectCreatesRestrictedRegistry(t *testing.T) {
	registry, err := NewRegistry(newStubTool("first"), newStubTool("second"), newStubTool("third"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	selected, err := registry.Select("third", "first", "third")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if got := selected.Names(); !reflect.DeepEqual(got, []string{"first", "third"}) {
		t.Fatalf("selected Names() = %v, want [first third]", got)
	}
	if _, ok := selected.Lookup("second"); ok {
		t.Fatal("selected registry contains omitted tool second")
	}

	empty, err := registry.Select()
	if err != nil {
		t.Fatalf("Select() empty error: %v", err)
	}
	if got := empty.Names(); len(got) != 0 {
		t.Fatalf("empty selection Names() = %v, want no tools", got)
	}

	if _, err := selected.Select("second"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("restricted Select(second) error = %v, want ErrToolNotFound", err)
	}
}

func TestSelectRejectsUnknownTool(t *testing.T) {
	registry, err := NewRegistry(newStubTool("known"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	if _, err := registry.Select("known", "unknown"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Select() error = %v, want ErrToolNotFound", err)
	}
}

func TestExecuteUsesSelectedTool(t *testing.T) {
	called := false
	callable := newStubTool("callable")
	callable.execute = func(_ context.Context, arguments json.RawMessage) (string, error) {
		called = true
		return string(arguments), nil
	}

	registry, err := NewRegistry(callable, newStubTool("blocked"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	selected, err := registry.Select("callable")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}

	result, err := selected.Execute(context.Background(), "callable", json.RawMessage(`{"value":1}`))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result != `{"value":1}` || !called {
		t.Fatalf("Execute() = %q, called = %v", result, called)
	}

	if _, err := selected.Execute(context.Background(), "blocked", json.RawMessage(`{}`)); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Execute(blocked) error = %v, want ErrToolNotFound", err)
	}
}

func TestExecuteRejectsNonObjectArguments(t *testing.T) {
	called := false
	tool := newStubTool("callable")
	tool.execute = func(context.Context, json.RawMessage) (string, error) {
		called = true
		return "", nil
	}

	registry, err := NewRegistry(tool)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	for _, arguments := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{"broken"`)} {
		if _, err := registry.Execute(context.Background(), "callable", arguments); err == nil {
			t.Errorf("Execute(%s) error = nil, want invalid arguments error", arguments)
		}
	}
	if called {
		t.Fatal("tool executed with invalid arguments")
	}
}
