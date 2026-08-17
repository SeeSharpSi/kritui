package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type stubCommand struct {
	definition Definition
	invocation Invocation
}

func (c *stubCommand) Definition() Definition {
	return c.definition
}

func (c *stubCommand) Execute(_ context.Context, invocation Invocation) (Result, error) {
	c.invocation = invocation
	return Result{}, nil
}

func TestParseRecognizesOnlyLeadingSlashCommands(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		want           Parsed
		wantCommand    bool
		wantParseError bool
	}{
		{name: "empty", input: ""},
		{name: "ordinary message", input: "hello"},
		{name: "leading space", input: " /new"},
		{name: "name", input: "/new", want: Parsed{Name: "new"}, wantCommand: true},
		{name: "arguments", input: "/rename New title", want: Parsed{Name: "rename", Arguments: "New title"}, wantCommand: true},
		{name: "tab separator", input: "/rename\t New title \t", want: Parsed{Name: "rename", Arguments: "New title"}, wantCommand: true},
		{name: "missing name", input: "/", wantCommand: true, wantParseError: true},
		{name: "uppercase name", input: "/New", wantCommand: true, wantParseError: true},
		{name: "invalid punctuation", input: "/new.chat", wantCommand: true, wantParseError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, isCommand, err := Parse(test.input)
			if isCommand != test.wantCommand {
				t.Fatalf("Parse() command = %t, want %t", isCommand, test.wantCommand)
			}
			if (err != nil) != test.wantParseError {
				t.Fatalf("Parse() error = %v, want error %t", err, test.wantParseError)
			}
			if test.wantParseError && !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("Parse() error = %v, want ErrInvalidCommand", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("Parse() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRegistryValidatesDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		command Command
		want    string
	}{
		{name: "nil", command: nil, want: "command is nil"},
		{name: "empty name", command: &stubCommand{definition: Definition{Description: "Description"}}, want: "command name is required"},
		{name: "invalid name", command: &stubCommand{definition: Definition{Name: "Bad", Description: "Description"}}, want: "lowercase letters"},
		{name: "empty description", command: &stubCommand{definition: Definition{Name: "valid"}}, want: "description is required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry(test.command)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewRegistry() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	definition := Definition{Name: "same", Description: "Description"}
	_, err := NewRegistry(&stubCommand{definition: definition}, &stubCommand{definition: definition})
	if err == nil || !strings.Contains(err.Error(), "duplicate command name") {
		t.Fatalf("NewRegistry() error = %v, want duplicate command name", err)
	}
}

func TestRegistryPreservesOrderAndExecutesCommand(t *testing.T) {
	first := &stubCommand{definition: Definition{Name: "first", Description: "First"}}
	second := &stubCommand{definition: Definition{Name: "second", Description: "Second"}}
	registry, err := NewRegistry(first, second)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("Names() = %v, want [first second]", got)
	}

	_, err = registry.Execute(context.Background(), Parsed{Name: "second", Arguments: "values"}, 42)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	want := Invocation{Name: "second", ChatID: 42, Arguments: "values"}
	if !reflect.DeepEqual(second.invocation, want) {
		t.Errorf("invocation = %#v, want %#v", second.invocation, want)
	}
}

func TestRegistryRejectsUnknownCommand(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	_, err = registry.Execute(context.Background(), Parsed{Name: "missing"}, 1)
	if !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("Execute() error = %v, want ErrCommandNotFound", err)
	}
}
