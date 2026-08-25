package commands

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/a-h/templ"
)

func execCommand(t *testing.T, command Command, parsed Parsed) (Result, error) {
	t.Helper()
	registry, err := NewRegistry(command)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}
	return registry.Execute(context.Background(), parsed, 0)
}

func TestRedirectCommand(t *testing.T) {
	command, err := NewRedirectCommand("new", "Start a new chat", "/")
	if err != nil {
		t.Fatalf("NewRedirectCommand() error: %v", err)
	}
	result, err := execCommand(t, command, Parsed{Name: "new"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.Status != http.StatusNoContent || result.Header.Get("HX-Redirect") != "/" {
		t.Errorf("result = %#v, want 204 redirect to /", result)
	}

	_, err = execCommand(t, command, Parsed{Name: "new", Arguments: "extra"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Status != http.StatusBadRequest {
		t.Fatalf("Execute(arguments) error = %v, want bad-request UserError", err)
	}
}

func TestPanelCommand(t *testing.T) {
	command, err := NewPanelCommand("history", "Open history", "history-page")
	if err != nil {
		t.Fatalf("NewPanelCommand() error: %v", err)
	}
	result, err := execCommand(t, command, Parsed{Name: "history"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.Status != http.StatusNoContent {
		t.Errorf("status = %d, want %d", result.Status, http.StatusNoContent)
	}
	if got := result.Header.Get("HX-Trigger"); got != `{"kritui:command":{"panel":"history-page"}}` {
		t.Errorf("HX-Trigger = %q", got)
	}
}

func TestRenameCommand(t *testing.T) {
	var gotChatID int64
	var gotTitle string
	command, err := NewRenameCommand(func(_ context.Context, chatID int64, title string) error {
		gotChatID = chatID
		gotTitle = title
		return nil
	})
	if err != nil {
		t.Fatalf("NewRenameCommand() error: %v", err)
	}
	if !command.definition.RequiresArguments {
		t.Errorf("definition.RequiresArguments = false, want true")
	}

	result, err := execCommand(t, command, Parsed{Name: "rename", Arguments: "New title"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotChatID != 0 || gotTitle != "New title" {
		t.Errorf("rename arguments = %d %q, want 0 New title", gotChatID, gotTitle)
	}
	if got := result.Header.Get("HX-Trigger"); got != `{"kritui:command":{}}` {
		t.Errorf("HX-Trigger = %q", got)
	}

	_, err = execCommand(t, command, Parsed{Name: "rename"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Message != "Usage: /rename <title>." {
		t.Fatalf("Execute(empty title) error = %v, want usage UserError", err)
	}
}

func TestMessageHistoryCommand(t *testing.T) {
	var gotChatID int64
	command, err := NewMessageHistoryCommand("undo", "Undo", func(_ context.Context, chatID int64) (templ.Component, error) {
		gotChatID = chatID
		return templ.Raw("changed"), nil
	})
	if err != nil {
		t.Fatalf("NewMessageHistoryCommand() error: %v", err)
	}

	result, err := execCommand(t, command, Parsed{Name: "undo"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotChatID != 0 {
		t.Errorf("chat ID = %d, want default zero injected by Execute", gotChatID)
	}
	if result.Status != http.StatusOK || result.Body == nil {
		t.Errorf("result = %#v, want 200 with body", result)
	}
	if got := result.Header.Get("HX-Retarget"); got != "#message-list" {
		t.Errorf("HX-Retarget = %q", got)
	}
	if got := result.Header.Get("HX-Reswap"); got != "outerHTML" {
		t.Errorf("HX-Reswap = %q", got)
	}
	if got := result.Header.Get("HX-Trigger-After-Settle"); got != `{"kritui:command":{"preserveInput":true}}` {
		t.Errorf("HX-Trigger-After-Settle = %q", got)
	}

	_, err = execCommand(t, command, Parsed{Name: "undo", Arguments: "extra"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Status != http.StatusBadRequest {
		t.Fatalf("Execute(arguments) error = %v, want bad-request UserError", err)
	}
}

func TestRegistryValidatesBuiltinDependencies(t *testing.T) {
	tests := []struct {
		name        string
		constructor func() (Command, error)
	}{
		{name: "redirect location", constructor: func() (Command, error) { return NewRedirectCommand("new", "New", "") }},
		{name: "panel ID", constructor: func() (Command, error) { return NewPanelCommand("history", "History", "") }},
		{name: "rename function", constructor: func() (Command, error) { return NewRenameCommand(nil) }},
		{name: "message history function", constructor: func() (Command, error) { return NewMessageHistoryCommand("undo", "Undo", nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, err := test.constructor()
			if err == nil {
				t.Fatal("constructor error = nil, want validation error")
			}
			if command.execute != nil {
				t.Errorf("invalid constructor returned populated command %#v", command)
			}
		})
	}
}
