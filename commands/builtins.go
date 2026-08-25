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

// RenameFunc persists a title for one chat.
type RenameFunc func(ctx context.Context, chatID int64, title string) error

// MessageHistoryFunc changes one chat's visible history and returns its
// server-rendered replacement.
type MessageHistoryFunc func(ctx context.Context, chatID int64) (templ.Component, error)

// NewRedirectCommand creates an argument-free command that redirects the page.
func NewRedirectCommand(name, description, location string) (Command, error) {
	if strings.TrimSpace(location) == "" {
		return Command{}, errors.New("commands: redirect location is required")
	}
	return NewCommand(
		Definition{Name: name, Description: description},
		func(_ context.Context, invocation Invocation) (Result, error) {
			if invocation.Arguments != "" {
				return Result{}, noArgumentsError(invocation.Name)
			}
			header := make(http.Header)
			header.Set("HX-Redirect", location)
			return Result{
				Status: http.StatusNoContent,
				Header: header,
			}, nil
		},
	)
}

// NewPanelCommand creates an argument-free command that opens an existing UI panel.
func NewPanelCommand(name, description, panelID string) (Command, error) {
	if strings.TrimSpace(panelID) == "" {
		return Command{}, errors.New("commands: panel ID is required")
	}
	return NewCommand(
		Definition{Name: name, Description: description},
		func(_ context.Context, invocation Invocation) (Result, error) {
			if invocation.Arguments != "" {
				return Result{}, noArgumentsError(invocation.Name)
			}
			return completedResult(panelID)
		},
	)
}

// NewRenameCommand creates /rename with injected persistence behavior.
func NewRenameCommand(rename RenameFunc) (Command, error) {
	if rename == nil {
		return Command{}, errors.New("commands: rename function is required")
	}
	return NewCommand(
		Definition{Name: "rename", Description: "Rename the current chat", RequiresArguments: true},
		func(ctx context.Context, invocation Invocation) (Result, error) {
			if invocation.Arguments == "" {
				return Result{}, &UserError{Status: http.StatusBadRequest, Message: "Usage: /rename <title>."}
			}
			if err := rename(ctx, invocation.ChatID, invocation.Arguments); err != nil {
				return Result{}, err
			}
			return completedResult("")
		},
	)
}

// NewMessageHistoryCommand creates an argument-free command that replaces the
// current message list after changing stored history.
func NewMessageHistoryCommand(name, description string, change MessageHistoryFunc) (Command, error) {
	if change == nil {
		return Command{}, errors.New("commands: message history function is required")
	}
	return NewCommand(
		Definition{Name: name, Description: description},
		func(ctx context.Context, invocation Invocation) (Result, error) {
			if invocation.Arguments != "" {
				return Result{}, noArgumentsError(invocation.Name)
			}
			body, err := change(ctx, invocation.ChatID)
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
		},
	)
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
