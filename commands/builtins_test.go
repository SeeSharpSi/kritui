package commands

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/a-h/templ"
)

func TestRedirectCommand(t *testing.T) {
	command := NewRedirectCommand("new", "Start a new chat", "/")
	result, err := command.Execute(context.Background(), Invocation{Name: "new"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.Status != http.StatusNoContent || result.Header.Get("HX-Redirect") != "/" {
		t.Errorf("result = %#v, want 204 redirect to /", result)
	}

	_, err = command.Execute(context.Background(), Invocation{Name: "new", Arguments: "extra"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Status != http.StatusBadRequest {
		t.Fatalf("Execute(arguments) error = %v, want bad-request UserError", err)
	}
}

func TestPanelCommand(t *testing.T) {
	command := NewPanelCommand("history", "Open history", "history-page")
	result, err := command.Execute(context.Background(), Invocation{Name: "history"})
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
	command := NewRenameCommand(func(_ context.Context, chatID int64, title string) error {
		gotChatID = chatID
		gotTitle = title
		return nil
	})
	if definition := command.Definition(); !definition.RequiresArguments {
		t.Errorf("Definition().RequiresArguments = false, want true")
	}

	result, err := command.Execute(context.Background(), Invocation{Name: "rename", ChatID: 7, Arguments: "New title"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotChatID != 7 || gotTitle != "New title" {
		t.Errorf("rename arguments = %d %q, want 7 New title", gotChatID, gotTitle)
	}
	if got := result.Header.Get("HX-Trigger"); got != `{"kritui:command":{}}` {
		t.Errorf("HX-Trigger = %q", got)
	}

	_, err = command.Execute(context.Background(), Invocation{Name: "rename"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Message != "Usage: /rename <title>." {
		t.Fatalf("Execute(empty title) error = %v, want usage UserError", err)
	}
}

func TestMessageHistoryCommand(t *testing.T) {
	var gotChatID int64
	command := NewMessageHistoryCommand("undo", "Undo", func(_ context.Context, chatID int64) (templ.Component, error) {
		gotChatID = chatID
		return templ.Raw("changed"), nil
	})

	result, err := command.Execute(context.Background(), Invocation{Name: "undo", ChatID: 7})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotChatID != 7 {
		t.Errorf("chat ID = %d, want 7", gotChatID)
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

	_, err = command.Execute(context.Background(), Invocation{Name: "undo", Arguments: "extra"})
	var userError *UserError
	if !errors.As(err, &userError) || userError.Status != http.StatusBadRequest {
		t.Fatalf("Execute(arguments) error = %v, want bad-request UserError", err)
	}
}

func TestRegistryValidatesBuiltinDependencies(t *testing.T) {
	tests := []struct {
		name    string
		command Command
	}{
		{name: "redirect location", command: NewRedirectCommand("new", "New", "")},
		{name: "panel ID", command: NewPanelCommand("history", "History", "")},
		{name: "rename function", command: NewRenameCommand(nil)},
		{name: "message history function", command: NewMessageHistoryCommand("undo", "Undo", nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.command); err == nil {
				t.Fatal("NewRegistry() error = nil, want validation error")
			}
		})
	}
}
