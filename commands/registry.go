// Package commands defines user-invoked slash commands and their registry.
package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/a-h/templ"
)

const maxCommandNameLength = 64

var (
	// ErrCommandNotFound indicates that a command is unavailable in a registry.
	ErrCommandNotFound = errors.New("commands: command not found")
	// ErrInvalidCommand indicates malformed slash-command input.
	ErrInvalidCommand = errors.New("commands: invalid command")

	commandNamePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// Definition describes a slash command to the application and user.
type Definition struct {
	Name              string
	Description       string
	RequiresArguments bool
}

// Parsed is command input separated from its leading slash.
type Parsed struct {
	Name      string
	Arguments string
}

// Invocation contains request-specific values supplied to a command.
type Invocation struct {
	Name      string
	ChatID    int64
	Arguments string
}

// Result describes the HTTP response produced by a command. Body, when set,
// must be a server-rendered HTML fragment suitable for the message form target.
type Result struct {
	Status int
	Header http.Header
	Body   templ.Component
}

// Command is one capability exposed through slash-command input.
type Command interface {
	Definition() Definition
	Execute(ctx context.Context, invocation Invocation) (Result, error)
}

// UserError is safe to display to the user with its HTTP status.
type UserError struct {
	Status  int
	Message string
}

func (e *UserError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type registeredCommand struct {
	definition Definition
	command    Command
}

// Registry is an immutable set of commands and is safe for concurrent use.
// Command implementations remain responsible for concurrency safety.
type Registry struct {
	byName map[string]registeredCommand
	order  []string
}

// NewRegistry creates a registry and validates every command definition.
func NewRegistry(commandList ...Command) (*Registry, error) {
	registry := &Registry{
		byName: make(map[string]registeredCommand, len(commandList)),
		order:  make([]string, 0, len(commandList)),
	}

	for index, command := range commandList {
		if isNilCommand(command) {
			return nil, fmt.Errorf("commands: register command at index %d: command is nil", index)
		}
		definition := command.Definition()
		if err := validateDefinition(definition); err != nil {
			return nil, fmt.Errorf("commands: register command at index %d: %w", index, err)
		}
		if validator, ok := command.(interface{ validate() error }); ok {
			if err := validator.validate(); err != nil {
				return nil, fmt.Errorf("commands: register %q: %w", definition.Name, err)
			}
		}
		if _, exists := registry.byName[definition.Name]; exists {
			return nil, fmt.Errorf("commands: register %q: duplicate command name", definition.Name)
		}

		registry.byName[definition.Name] = registeredCommand{definition: definition, command: command}
		registry.order = append(registry.order, definition.Name)
	}

	return registry, nil
}

// Parse recognizes input only when its first byte is an ASCII forward slash.
// Arguments remain a single string so each command can define its own grammar.
func Parse(input string) (Parsed, bool, error) {
	if input == "" || input[0] != '/' {
		return Parsed{}, false, nil
	}

	remainder := input[1:]
	boundary := strings.IndexFunc(remainder, unicode.IsSpace)
	name := remainder
	arguments := ""
	if boundary >= 0 {
		name = remainder[:boundary]
		arguments = strings.TrimSpace(remainder[boundary:])
	}
	if name == "" || len(name) > maxCommandNameLength || !commandNamePattern.MatchString(name) {
		return Parsed{}, true, fmt.Errorf("%w: command names may contain only lowercase letters, digits, underscores, and hyphens", ErrInvalidCommand)
	}

	return Parsed{Name: name, Arguments: arguments}, true, nil
}

// Names returns registered command names in registration order.
func (r *Registry) Names() []string {
	return slices.Clone(r.order)
}

// Definitions returns command definitions in registration order.
func (r *Registry) Definitions() []Definition {
	definitions := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definitions = append(definitions, r.byName[name].definition)
	}
	return definitions
}

// Lookup returns a registered command by name.
func (r *Registry) Lookup(name string) (Command, bool) {
	registered, ok := r.byName[name]
	return registered.command, ok
}

// Execute invokes a registered command.
func (r *Registry) Execute(ctx context.Context, parsed Parsed, chatID int64) (Result, error) {
	registered, exists := r.byName[parsed.Name]
	if !exists {
		return Result{}, fmt.Errorf("%w: %q", ErrCommandNotFound, parsed.Name)
	}

	return registered.command.Execute(ctx, Invocation{
		Name:      parsed.Name,
		ChatID:    chatID,
		Arguments: parsed.Arguments,
	})
}

func validateDefinition(definition Definition) error {
	if definition.Name == "" {
		return errors.New("command name is required")
	}
	if len(definition.Name) > maxCommandNameLength {
		return fmt.Errorf("command name %q exceeds %d characters", definition.Name, maxCommandNameLength)
	}
	if !commandNamePattern.MatchString(definition.Name) {
		return fmt.Errorf("command name %q may contain only lowercase letters, digits, underscores, and hyphens", definition.Name)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return fmt.Errorf("command %q description is required", definition.Name)
	}
	return nil
}

func isNilCommand(command Command) bool {
	if command == nil {
		return true
	}

	value := reflect.ValueOf(command)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
