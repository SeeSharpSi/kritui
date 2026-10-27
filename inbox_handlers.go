package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/tools"
)

type promptAdmission struct {
	requestID string
	model     string
	tools     []string
	messages  []llm.Message
	inbox     []kritui_db.InboxItem
}

func admitPrompt(ctx context.Context, database *sql.DB, request messageRequest, store *toolCallStore, content string, attachments []llm.UserAttachment, appendIDs []string, submissionID, delivery string) (promptAdmission, error) {
	store.workMu.Lock()
	defer store.workMu.Unlock()
	if submissionID == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return promptAdmission{}, err
		}
		submissionID = hex.EncodeToString(token[:])
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return promptAdmission{}, err
	}
	defer tx.Rollback()
	if err := validateMessageMCPTools(ctx, tx, request.toolNames); err != nil {
		return promptAdmission{}, err
	}
	appends, err := selectMessageAppends(ctx, tx, content, appendIDs)
	if err != nil {
		return promptAdmission{}, err
	}
	item, err := kritui_db.EnqueuePrompt(ctx, tx, request.chatID, kritui_db.InboxItem{
		SubmissionID: submissionID, Delivery: delivery, Model: request.model,
		Tools: request.toolNames, AppendIDs: kritui_db.PromptAppendIDs(appends),
		Message: llm.Message{Role: "user", Content: content, Attachments: attachments, PromptAppendTexts: promptAppendTexts(appends)},
	})
	if err != nil {
		return promptAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return promptAdmission{}, err
	}
	items, err := kritui_db.GetInbox(ctx, database, request.chatID)
	if err != nil {
		return promptAdmission{}, err
	}
	admission := promptAdmission{inbox: items}
	if active, ok := store.activeForUser(kritui_db.UserID(ctx), request.chatID); ok {
		if tracker, exists := store.getForUser(kritui_db.UserID(ctx), active.requestID); exists {
			messages, err := kritui_db.GetMessages(ctx, database, request.chatID)
			if err != nil {
				return promptAdmission{}, err
			}
			tracker.present(renderCompletionFragment(ctx, templates.ActiveCompletionResult(request.chat, messages, items)))
		}
		return admission, nil
	}
	if item.Status != "pending" {
		return admission, nil
	}
	snapshot, err := kritui_db.GetMessageSnapshot(ctx, database, request.chatID)
	if err != nil {
		return promptAdmission{}, err
	}
	state, err := kritui_db.GetCompletionState(ctx, database, request.chatID)
	if err != nil {
		return promptAdmission{}, err
	}
	if state.Pending {
		request.model, request.toolNames = state.Model, state.Tools
	} else if len(snapshot.Messages) == 0 || snapshot.Messages[len(snapshot.Messages)-1].Role != "user" {
		state = nextInboxState(items)
		checkpoint, err := kritui_db.CheckpointCompletionWithState(ctx, database, request.chatID, snapshot, nil, "input", state)
		if err != nil {
			return promptAdmission{}, err
		}
		snapshot = checkpoint.Snapshot
		if len(checkpoint.Delivered) > 0 {
			first := checkpoint.Delivered[0]
			request.model, request.toolNames = first.Model, first.Tools
			if err := kritui_db.UpsertChat(ctx, database, request.chatID, normalizeChatTitle(first.Message.Content), first.Tools, first.AppendIDs); err != nil {
				return promptAdmission{}, err
			}
		}
	}
	if err := kritui_db.SetCompletionState(ctx, database, request.chatID, kritui_db.CompletionState{Pending: true, Model: request.model, Tools: request.toolNames}); err != nil {
		return promptAdmission{}, err
	}
	id, err := store.createForUser(kritui_db.UserID(ctx), request.chatID, request.model, request.toolNames)
	if err != nil {
		return promptAdmission{}, err
	}
	admission.requestID, admission.model, admission.tools = id, request.model, request.toolNames
	admission.messages = snapshot.Messages
	admission.inbox, err = kritui_db.GetInbox(ctx, database, request.chatID)
	return admission, err
}

func requestForInbox(registry *tools.Registry, chatID int64, model string, names []string) (messageRequest, error) {
	var builtins, remote []string
	for _, name := range names {
		if _, ok := kritui_db.ParseMCPServerCapability(name); ok {
			remote = append(remote, name)
		} else {
			builtins = append(builtins, name)
		}
	}
	selected, err := registry.Select(builtins...)
	if err != nil {
		return messageRequest{}, err
	}
	return messageRequest{chat: strconv.FormatInt(chatID, 10), chatID: chatID, model: model, toolNames: names, selected: selected, mcpTools: remote}, nil
}

// runMessageCompletion owns an entire busy period. Follow-up turns start here,
// independently of SSE subscribers or the browser that submitted them.
func runMessageCompletion(ctx context.Context, database *sql.DB, registry *tools.Registry, request messageRequest, requestID string, tracker *toolCallTracker, store *toolCallStore, logger *log.Logger, timezone string) {
	settled := false
	terminal := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("complete message panic: %v", recovered)
			terminal = renderCompletionFragment(ctx, templates.CompletionError(request.chat, "Failed to complete message.", request.model, request.toolNames))
		}
		if !settled {
			store.workMu.Lock()
			defer store.workMu.Unlock()
			messages, _ := kritui_db.GetMessages(ctx, database, request.chatID)
			inbox, _ := kritui_db.GetInbox(ctx, database, request.chatID)
			store.finish(requestID, renderCompletionFragment(ctx, templates.CompletionResult(request.chat, messages, inbox))+terminal)
		}
	}()
	if err := kritui_db.SetCompletionState(ctx, database, request.chatID, kritui_db.CompletionState{Pending: true, Model: request.model, Tools: request.toolNames}); err != nil {
		terminal = renderCompletionFragment(ctx, templates.MessageError("Failed to prepare completion."))
		return
	}
	for {
		var succeeded bool
		terminal, succeeded = runMessageCompletionTurn(ctx, database, request, tracker, store, logger, timezone)
		if !succeeded {
			return
		}
		store.workMu.Lock()
		snapshot, err := kritui_db.GetMessageSnapshot(ctx, database, request.chatID)
		var next kritui_db.CheckpointResult
		if err == nil {
			next, err = kritui_db.CheckpointCompletionWithState(ctx, database, request.chatID, snapshot, nil, "input", kritui_db.CompletionState{Model: request.model, Tools: request.toolNames})
		}
		if err != nil {
			store.workMu.Unlock()
			terminal = renderCompletionFragment(ctx, templates.CompletionError(request.chat, "Failed to advance prompt inbox.", request.model, request.toolNames))
			return
		}
		if len(next.Delivered) == 0 {
			err = kritui_db.SetCompletionState(ctx, database, request.chatID, kritui_db.CompletionState{Pending: false, Model: request.model, Tools: request.toolNames})
			if err == nil {
				store.finish(requestID, renderCompletionFragment(ctx, templates.CompletionResult(request.chat, next.Snapshot.Messages, nil)))
				settled = true
			}
			store.workMu.Unlock()
			if err != nil {
				terminal = renderCompletionFragment(ctx, templates.CompletionError(request.chat, "Failed to finish completion.", request.model, request.toolNames))
			}
			return
		}
		item := next.Delivered[0]
		if item.Delivery == "queue" {
			var nextRequest messageRequest
			nextRequest, err = requestForInbox(registry, request.chatID, item.Model, item.Tools)
			if err == nil {
				request = nextRequest
			}
		}
		if err == nil {
			err = kritui_db.SetCompletionState(ctx, database, request.chatID, kritui_db.CompletionState{Pending: true, Model: request.model, Tools: request.toolNames})
		}
		if err == nil {
			store.setOptions(requestID, request.model, request.toolNames)
			inbox, inboxErr := kritui_db.GetInbox(ctx, database, request.chatID)
			err = inboxErr
			if err == nil {
				tracker.presentCheckpoint(ctx, request.chat, next.Snapshot.Messages, inbox)
			}
		}
		store.workMu.Unlock()
		if err != nil {
			terminal = renderCompletionFragment(ctx, templates.CompletionError(request.chat, "Failed to configure queued prompt.", request.model, request.toolNames))
			return
		}
	}
}

func inboxMutationHandler(database *sql.DB, registry *tools.Registry, store *toolCallStore, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		inputID, inputOK := positiveID(r.PathValue("input"))
		if !ok || !inputOK {
			renderInboxMutationError(w, r, http.StatusBadRequest, "A valid chat and pending prompt are required.")
			return
		}
		if err := parseLimitedForm(w, r, maxRetryBodyBytes); err != nil {
			renderInboxMutationError(w, r, http.StatusBadRequest, "Invalid pending prompt request.")
			return
		}
		store.workMu.Lock()
		defer store.workMu.Unlock()
		var err error
		if r.Method == http.MethodDelete {
			err = kritui_db.CancelInboxPrompt(r.Context(), database, chatID, inputID)
		} else {
			delivery := r.FormValue("delivery")
			if delivery != "steer" && delivery != "queue" {
				renderInboxMutationError(w, r, http.StatusBadRequest, "Prompt delivery must be steer or queue.")
				return
			}
			err = kritui_db.SetInboxDelivery(r.Context(), database, chatID, inputID, delivery)
		}
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, kritui_db.ErrChatNotFound) {
				status = http.StatusNotFound
			}
			if errors.Is(err, kritui_db.ErrInboxConflict) {
				status = http.StatusConflict
			}
			renderInboxMutationError(w, r, status, "Pending prompt is no longer available.")
			if errors.Is(err, kritui_db.ErrInboxConflict) {
				if items, loadErr := kritui_db.GetInbox(r.Context(), database, chatID); loadErr == nil {
					_ = templates.InboxCheckpoint(strconv.FormatInt(chatID, 10), items).Render(r.Context(), w)
				}
			}
			return
		}
		items, err := kritui_db.GetInbox(r.Context(), database, chatID)
		if err != nil {
			renderInboxMutationError(w, r, http.StatusInternalServerError, "Failed to load pending prompts.")
			return
		}
		active, running := store.activeForUser(kritui_db.UserID(r.Context()), chatID)
		if !running && r.Method == http.MethodPatch && r.FormValue("delivery") == "steer" {
			resumed, err := startInboxLocked(r.Context(), database, registry, store, logger, chatID, r.FormValue("client_timezone"))
			if err != nil {
				renderInboxMutationError(w, r, http.StatusConflict, "Failed to resume pending input.")
				return
			}
			items, _ = kritui_db.GetInbox(r.Context(), database, chatID)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("HX-Retarget", "#messages")
			w.Header().Set("HX-Reswap", "beforeend")
			_ = templates.ClearCompletionErrors().Render(r.Context(), w)
			_ = templates.ActiveCompletionResult(resumed.request.chat, resumed.snapshot.Messages, items).Render(r.Context(), w)
			_ = templates.CompletionWithHistory(resumed.request.chat, resumed.requestID, resumed.request.model, resumed.request.toolNames, false, resumed.snapshot.Messages).Render(r.Context(), w)
			return
		}
		if running {
			if tracker, exists := store.getForUser(kritui_db.UserID(r.Context()), active.requestID); exists {
				messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
				if err == nil {
					tracker.present(renderCompletionFragment(r.Context(), templates.ActiveCompletionResult(strconv.FormatInt(chatID, 10), messages, items)))
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.PromptInbox(strconv.FormatInt(chatID, 10), items).Render(r.Context(), w)
	}
}

func renderInboxMutationError(w http.ResponseWriter, r *http.Request, status int, message string) {
	// Inbox controls replace their own panel on success. Error fragments belong
	// in the transcript so a failed mutation cannot remove the SSE update target.
	w.Header().Set("HX-Retarget", "#messages")
	w.Header().Set("HX-Reswap", "beforeend")
	renderMessageError(w, r, status, message)
}

func inboxResumeHandler(database *sql.DB, registry *tools.Registry, store *toolCallStore, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			renderMessageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		if !parseMessageForm(w, r, maxRetryBodyBytes) {
			return
		}
		store.workMu.Lock()
		defer store.workMu.Unlock()
		owned, err := requireOwnedChat(r.Context(), database, chatID)
		if err != nil || !owned {
			renderMessageError(w, r, http.StatusNotFound, "Chat not found.")
			return
		}
		if _, active := store.activeForUser(kritui_db.UserID(r.Context()), chatID); active {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		resumed, err := startInboxLocked(r.Context(), database, registry, store, logger, chatID, r.FormValue("client_timezone"))
		if err != nil {
			log.Printf("resume prompt inbox: %v", err)
			renderMessageError(w, r, http.StatusConflict, "No prompt is available to resume.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		items, _ := kritui_db.GetInbox(r.Context(), database, chatID)
		_ = templates.ClearCompletionErrors().Render(r.Context(), w)
		_ = templates.ActiveCompletionResult(resumed.request.chat, resumed.snapshot.Messages, items).Render(r.Context(), w)
		_ = templates.CompletionWithHistory(resumed.request.chat, resumed.requestID, resumed.request.model, resumed.request.toolNames, false, resumed.snapshot.Messages).Render(r.Context(), w)
	}
}

type resumedPrompt struct {
	request   messageRequest
	requestID string
	snapshot  kritui_db.MessageSnapshot
}

// startInboxLocked starts parked work while the caller owns workMu.
func startInboxLocked(ctx context.Context, database *sql.DB, registry *tools.Registry, store *toolCallStore, logger *log.Logger, chatID int64, timezone string) (resumedPrompt, error) {
	state, err := kritui_db.GetCompletionState(ctx, database, chatID)
	if err != nil {
		return resumedPrompt{}, err
	}
	snapshot, err := kritui_db.GetMessageSnapshot(ctx, database, chatID)
	if err != nil {
		return resumedPrompt{}, err
	}
	if !state.Pending {
		if len(snapshot.Messages) > 0 && snapshot.Messages[len(snapshot.Messages)-1].Role == "user" {
			state.Model, err = kritui_db.GetDefaultModel(ctx, database, os.Getenv("LLM_MODEL"))
			if err == nil {
				state.Tools, err = kritui_db.GetChatTools(ctx, database, chatID)
			}
			if err != nil {
				return resumedPrompt{}, err
			}
		} else {
			items, err := kritui_db.GetInbox(ctx, database, chatID)
			if err != nil {
				return resumedPrompt{}, err
			}
			state = nextInboxState(items)
			checkpoint, err := kritui_db.CheckpointCompletionWithState(ctx, database, chatID, snapshot, nil, "input", state)
			if err != nil {
				return resumedPrompt{}, err
			}
			if len(checkpoint.Delivered) == 0 {
				return resumedPrompt{}, kritui_db.ErrInboxConflict
			}
			snapshot = checkpoint.Snapshot
			state.Model, state.Tools = checkpoint.Delivered[0].Model, checkpoint.Delivered[0].Tools
		}
	}
	request, err := requestForInbox(registry, chatID, state.Model, state.Tools)
	if err != nil {
		return resumedPrompt{}, err
	}
	state.Pending = true
	if err := kritui_db.SetCompletionState(ctx, database, chatID, state); err != nil {
		return resumedPrompt{}, err
	}
	id, err := store.createForUser(kritui_db.UserID(ctx), chatID, request.model, request.toolNames)
	if err != nil {
		return resumedPrompt{}, err
	}
	tracker, claimed := store.claimForUser(kritui_db.UserID(ctx), id, chatID, request.model, request.toolNames)
	if !claimed {
		store.delete(id)
		return resumedPrompt{}, errChatCompletionActive
	}
	completionContext := kritui_db.WithUser(context.WithoutCancel(ctx), kritui_db.UserID(ctx))
	go runMessageCompletion(completionContext, database, registry, request, id, tracker, store, logger, timezone)
	return resumedPrompt{request: request, requestID: id, snapshot: snapshot}, nil
}

func nextInboxState(items []kritui_db.InboxItem) kritui_db.CompletionState {
	if len(items) == 0 {
		return kritui_db.CompletionState{}
	}
	first := items[0]
	for _, item := range items {
		if item.Delivery == "steer" {
			first = item
			break
		}
	}
	return kritui_db.CompletionState{Pending: true, Model: first.Model, Tools: first.Tools}
}

func (s *toolCallStore) setOptions(id, model string, tools []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[id]; ok {
		entry.model = model
		entry.tools = append([]string(nil), tools...)
	}
}
