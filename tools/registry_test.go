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

type groupedTool struct {
	stubTool
	capability string
}

func (t groupedTool) Capability() string {
	return t.capability
}

func newGroupedTool(name, capability string) groupedTool {
	return groupedTool{stubTool: newStubTool(name), capability: capability}
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

func TestNewRegistryRejectsInvalidCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		want       string
	}{
		{name: "empty", capability: "", want: "capability name is required"},
		{name: "invalid", capability: "bad name", want: "may contain only"},
		{name: "long", capability: strings.Repeat("a", 65), want: "exceeds 64"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(newGroupedTool("tool", test.capability)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewRegistry() error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestNewRegistryRejectsAmbiguousCapabilities(t *testing.T) {
	ordinary := newStubTool("search")
	grouped := newGroupedTool("fetch", "search")

	if _, err := NewRegistry(ordinary, grouped); err == nil || !strings.Contains(err.Error(), "may be shared only by CapabilityTool") {
		t.Fatalf("NewRegistry(ordinary, grouped) error = %v, want capability collision", err)
	}
	if _, err := NewRegistry(grouped, ordinary); err == nil || !strings.Contains(err.Error(), "may be shared only by CapabilityTool") {
		t.Fatalf("NewRegistry(grouped, ordinary) error = %v, want capability collision", err)
	}
}

func TestCapabilityToolGroupsNamesAndDefinitions(t *testing.T) {
	registry, err := NewRegistry(
		newGroupedTool("search_alpha", "search"),
		newStubTool("web"),
		newGroupedTool("search_beta", "search"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	if got := registry.Names(); !reflect.DeepEqual(got, []string{"search", "web"}) {
		t.Fatalf("Names() = %v, want [search web]", got)
	}
	if got := toolNames(registry.Definitions()); !reflect.DeepEqual(got, []string{"search_alpha", "web", "search_beta"}) {
		t.Fatalf("Definitions() names = %v, want [search_alpha web search_beta]", got)
	}
	if _, ok := registry.Lookup("search_beta"); !ok {
		t.Fatal("Lookup(search_beta) missing")
	}
	if !registry.HasCapability("search") {
		t.Fatal("HasCapability(search) = false, want true")
	}
	if registry.HasCapability("search_alpha") {
		t.Fatal("HasCapability(search_alpha) = true, want false (function name, not capability)")
	}
}

func TestSelectCapabilityEnablesGroupedTools(t *testing.T) {
	registry, err := NewRegistry(
		newGroupedTool("search_alpha", "search"),
		newStubTool("web"),
		newGroupedTool("search_beta", "search"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	selected, err := registry.Select("search", "web", "search")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if got := selected.Names(); !reflect.DeepEqual(got, []string{"search", "web"}) {
		t.Fatalf("selected Names() = %v, want [search web]", got)
	}
	if got := toolNames(selected.Definitions()); !reflect.DeepEqual(got, []string{"search_alpha", "web", "search_beta"}) {
		t.Fatalf("selected Definitions() names = %v, want [search_alpha web search_beta]", got)
	}
	for _, name := range []string{"search_alpha", "web", "search_beta"} {
		if _, err := selected.Execute(context.Background(), name, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Execute(%s) error: %v", name, err)
		}
	}

	groupOnly, err := registry.Select("search")
	if err != nil {
		t.Fatalf("Select(search) error: %v", err)
	}
	if got := groupOnly.Names(); !reflect.DeepEqual(got, []string{"search"}) {
		t.Fatalf("groupOnly Names() = %v, want [search]", got)
	}
	if _, err := groupOnly.Execute(context.Background(), "search_alpha", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute(search_alpha) error: %v", err)
	}
	if _, err := groupOnly.Execute(context.Background(), "search_beta", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute(search_beta) error: %v", err)
	}
	if _, err := groupOnly.Execute(context.Background(), "web", json.RawMessage(`{}`)); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Execute(web) error = %v, want ErrToolNotFound", err)
	}
	if groupOnly.HasCapability("web") {
		t.Fatal("groupOnly HasCapability(web) = true, want false")
	}

	webOnly, err := registry.Select("web")
	if err != nil {
		t.Fatalf("Select(web) error: %v", err)
	}
	if _, err := webOnly.Execute(context.Background(), "web", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute(web) error: %v", err)
	}
	if _, err := webOnly.Execute(context.Background(), "search_alpha", json.RawMessage(`{}`)); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Execute(search_alpha) error = %v, want ErrToolNotFound", err)
	}

	if _, err := registry.Select("search_alpha"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Select(search_alpha) error = %v, want ErrToolNotFound", err)
	}
}

func TestWithAppendsToolsWithoutMutatingReceiver(t *testing.T) {
	appended := newStubTool("appended")
	appended.execute = func(_ context.Context, arguments json.RawMessage) (string, error) {
		return string(arguments), nil
	}

	registry, err := NewRegistry(newStubTool("base"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	before := registry.Names()

	combined, err := registry.With(appended)
	if err != nil {
		t.Fatalf("With() error: %v", err)
	}
	if got := combined.Names(); !reflect.DeepEqual(got, []string{"base", "appended"}) {
		t.Fatalf("combined Names() = %v, want [base appended]", got)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, before) {
		t.Fatalf("receiver Names() = %v, want unchanged %v", got, before)
	}
	if _, ok := registry.Lookup("appended"); ok {
		t.Fatal("receiver contains appended tool")
	}

	result, err := combined.Execute(context.Background(), "appended", json.RawMessage(`{"ok":true}`))
	if err != nil || result != `{"ok":true}` {
		t.Fatalf("Execute(appended) = %q, error: %v", result, err)
	}
	selected, err := combined.Select("appended")
	if err != nil {
		t.Fatalf("Select(appended) error: %v", err)
	}
	if got := selected.Names(); !reflect.DeepEqual(got, []string{"appended"}) {
		t.Fatalf("selected Names() = %v, want [appended]", got)
	}
}

func TestWithFromSelectedRegistry(t *testing.T) {
	registry, err := NewRegistry(newStubTool("first"), newStubTool("second"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	selected, err := registry.Select("second")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}

	combined, err := selected.With(newStubTool("third"))
	if err != nil {
		t.Fatalf("With() error: %v", err)
	}
	if got := combined.Names(); !reflect.DeepEqual(got, []string{"second", "third"}) {
		t.Fatalf("combined Names() = %v, want [second third]", got)
	}
	if _, ok := combined.Lookup("first"); ok {
		t.Fatal("combined registry contains unselected tool first")
	}
	if got := selected.Names(); !reflect.DeepEqual(got, []string{"second"}) {
		t.Fatalf("selected Names() = %v, want unchanged [second]", got)
	}
}

func TestWithRejectsConflictsWithoutMutatingReceiver(t *testing.T) {
	registry, err := NewRegistry(newStubTool("search"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	before := registry.Names()

	if _, err := registry.With(newGroupedTool("fetch", "search")); err == nil || !strings.Contains(err.Error(), "may be shared only by CapabilityTool") {
		t.Fatalf("With(ordinary name, grouped capability) error = %v, want capability collision", err)
	}
	if _, err := registry.With(newStubTool("search")); err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("With(duplicate) error = %v, want duplicate tool name", err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, before) {
		t.Fatalf("receiver Names() = %v, want unchanged %v", got, before)
	}

	grouped, err := NewRegistry(newGroupedTool("search_alpha", "search"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	if _, err := grouped.With(newStubTool("search")); err == nil || !strings.Contains(err.Error(), "may be shared only by CapabilityTool") {
		t.Fatalf("With(grouped capability, ordinary name) error = %v, want capability collision", err)
	}
}

func TestWithNoArgumentsReturnsEquivalentRegistry(t *testing.T) {
	registry, err := NewRegistry(newStubTool("first"), newGroupedTool("search_alpha", "search"))
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	same, err := registry.With()
	if err != nil {
		t.Fatalf("With() error: %v", err)
	}
	if same == registry {
		t.Fatal("With() returned the receiver")
	}
	if got := same.Names(); !reflect.DeepEqual(got, registry.Names()) {
		t.Fatalf("Names() = %v, want %v", got, registry.Names())
	}
	if got := same.Definitions(); !reflect.DeepEqual(got, registry.Definitions()) {
		t.Fatalf("Definitions() = %v, want %v", got, registry.Definitions())
	}
}

func toolNames(definitions []Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
