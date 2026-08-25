// Package commands defines user-invoked slash commands and their registry.
package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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

// Command couples one validated definition with its execute behavior.
type Command struct {
	definition Definition
	execute    func(ctx context.Context, invocation Invocation) (Result, error)
}

// NewCommand validates the definition and binds it to an execute function so
// dependency problems surface at construction instead of registration time.
func NewCommand(definition Definition, execute func(context.Context, Invocation) (Result, error)) (Command, error) {
	if err := validateDefinition(definition); err != nil {
		return Command{}, err
	}
	if execute == nil {
		return Command{}, errors.New("commands: execute function is required")
	}
	return Command{definition: definition, execute: execute}, nil
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

// Registry is an immutable set of commands and is safe for concurrent use.
// Command implementations remain responsible for concurrency safety.
type Registry struct {
	byName map[string]Command
	order  []string
}

// NewRegistry creates a registry and revalidates every command definition so
// directly constructed Command values cannot bypass validation.
func NewRegistry(commandList ...Command) (*Registry, error) {
	registry := &Registry{
		byName: make(map[string]Command, len(commandList)),
		order:  make([]string, 0, len(commandList)),
	}

	for index, command := range commandList {
		if command.execute == nil {
			return nil, fmt.Errorf("commands: register command at index %d: command is nil", index)
		}
		if err := validateDefinition(command.definition); err != nil {
			return nil, fmt.Errorf("commands: register command at index %d: %w", index, err)
		}
		if _, exists := registry.byName[command.definition.Name]; exists {
			return nil, fmt.Errorf("commands: register %q: duplicate command name", command.definition.Name)
		}

		registry.byName[command.definition.Name] = command
		registry.order = append(registry.order, command.definition.Name)
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

// Definitions returns command definitions in registration order.
func (r *Registry) Definitions() []Definition {
	definitions := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definitions = append(definitions, r.byName[name].definition)
	}
	return definitions
}

// Execute invokes a registered command.
func (r *Registry) Execute(ctx context.Context, parsed Parsed, chatID int64) (Result, error) {
	registered, exists := r.byName[parsed.Name]
	if !exists {
		return Result{}, fmt.Errorf("%w: %q", ErrCommandNotFound, parsed.Name)
	}

	return registered.execute(ctx, Invocation{
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
