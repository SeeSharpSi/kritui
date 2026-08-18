package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/a-h/templ"
)

const commandEvent = "kritui:command"

type redirectCommand struct {
	definition Definition
	location   string
}

// NewRedirectCommand creates an argument-free command that redirects the page.
func NewRedirectCommand(name, description, location string) Command {
	return &redirectCommand{
		definition: Definition{Name: name, Description: description},
		location:   location,
	}
}

func (c *redirectCommand) Definition() Definition {
	return c.definition
}

func (c *redirectCommand) Execute(_ context.Context, invocation Invocation) (Result, error) {
	if invocation.Arguments != "" {
		return Result{}, noArgumentsError(invocation.Name)
	}
	header := make(http.Header)
	header.Set("HX-Redirect", c.location)
	return Result{
		Status: http.StatusNoContent,
		Header: header,
	}, nil
}

func (c *redirectCommand) validate() error {
	if strings.TrimSpace(c.location) == "" {
		return errors.New("redirect location is required")
	}
	return nil
}

type panelCommand struct {
	definition Definition
	panelID    string
}

// NewPanelCommand creates an argument-free command that opens an existing UI panel.
func NewPanelCommand(name, description, panelID string) Command {
	return &panelCommand{
		definition: Definition{Name: name, Description: description},
		panelID:    panelID,
	}
}

func (c *panelCommand) Definition() Definition {
	return c.definition
}

func (c *panelCommand) Execute(_ context.Context, invocation Invocation) (Result, error) {
	if invocation.Arguments != "" {
		return Result{}, noArgumentsError(invocation.Name)
	}
	return completedResult(c.panelID)
}

func (c *panelCommand) validate() error {
	if strings.TrimSpace(c.panelID) == "" {
		return errors.New("panel ID is required")
	}
	return nil
}

// RenameFunc persists a title for one chat.
type RenameFunc func(ctx context.Context, chatID int64, title string) error

type renameCommand struct {
	rename RenameFunc
}

// NewRenameCommand creates /rename with injected persistence behavior.
func NewRenameCommand(rename RenameFunc) Command {
	return &renameCommand{rename: rename}
}

func (c *renameCommand) Definition() Definition {
	return Definition{Name: "rename", Description: "Rename the current chat", RequiresArguments: true}
}

func (c *renameCommand) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Arguments == "" {
		return Result{}, &UserError{Status: http.StatusBadRequest, Message: "Usage: /rename <title>."}
	}
	if err := c.rename(ctx, invocation.ChatID, invocation.Arguments); err != nil {
		return Result{}, err
	}
	return completedResult("")
}

func (c *renameCommand) validate() error {
	if c.rename == nil {
		return errors.New("rename function is required")
	}
	return nil
}

// MessageHistoryFunc changes one chat's visible history and returns its
// server-rendered replacement.
type MessageHistoryFunc func(ctx context.Context, chatID int64) (templ.Component, error)

type messageHistoryCommand struct {
	definition Definition
	change     MessageHistoryFunc
}

// NewMessageHistoryCommand creates an argument-free command that replaces the
// current message list after changing stored history.
func NewMessageHistoryCommand(name, description string, change MessageHistoryFunc) Command {
	return &messageHistoryCommand{
		definition: Definition{Name: name, Description: description},
		change:     change,
	}
}

func (c *messageHistoryCommand) Definition() Definition {
	return c.definition
}

func (c *messageHistoryCommand) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Arguments != "" {
		return Result{}, noArgumentsError(invocation.Name)
	}
	body, err := c.change(ctx, invocation.ChatID)
	if err != nil {
		return Result{}, err
	}
	trigger, err := encodeCommandEvent(map[string]any{"preserveInput": true})
	if err != nil {
		return Result{}, err
	}
	header := make(http.Header)
	header.Set("HX-Retarget", "#message-list")
	header.Set("HX-Reswap", "outerHTML")
	header.Set("HX-Trigger-After-Settle", trigger)
	return Result{Status: http.StatusOK, Header: header, Body: body}, nil
}

func (c *messageHistoryCommand) validate() error {
	if c.change == nil {
		return errors.New("message history function is required")
	}
	return nil
}

func completedResult(panelID string) (Result, error) {
	detail := map[string]any{}
	if panelID != "" {
		detail["panel"] = panelID
	}
	trigger, err := encodeCommandEvent(detail)
	if err != nil {
		return Result{}, err
	}
	header := make(http.Header)
	header.Set("HX-Trigger", trigger)
	return Result{
		Status: http.StatusNoContent,
		Header: header,
	}, nil
}

func encodeCommandEvent(detail map[string]any) (string, error) {
	trigger, err := json.Marshal(map[string]any{commandEvent: detail})
	if err != nil {
		return "", fmt.Errorf("commands: encode completion event: %w", err)
	}
	return string(trigger), nil
}

func noArgumentsError(name string) error {
	return &UserError{
		Status:  http.StatusBadRequest,
		Message: fmt.Sprintf("/%s does not accept arguments.", name),
	}
}
