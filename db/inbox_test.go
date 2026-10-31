package kritui_db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func inboxTestContext() context.Context {
	return WithUser(context.Background(), LegacyOwnerID)
}

func enqueueInboxTestItem(t *testing.T, database *sql.DB, ctx context.Context, chatID int64, item InboxItem) InboxItem {
	t.Helper()
	stored, err := EnqueuePrompt(ctx, database, chatID, item)
	if err != nil {
		t.Fatalf("EnqueuePrompt(%q) error: %v", item.SubmissionID, err)
	}
	return stored
}

func TestInboxChatAllocationPreservesParkedWork(t *testing.T) {
	for _, pendingState := range []bool{false, true} {
		name := "pending inbox"
		if pendingState {
			name = "pending completion"
		}
		t.Run(name, func(t *testing.T) {
			database := openMessagesTestDatabase(t, "")
			ctx := inboxTestContext()
			if _, err := database.Exec(`INSERT INTO chats (id, created_at) VALUES (1, '2000-01-01T00:00:00.000Z')`); err != nil {
				t.Fatal(err)
			}
			var item InboxItem
			if pendingState {
				if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "parked-model"}); err != nil {
					t.Fatal(err)
				}
			} else {
				item = enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
					SubmissionID: "parked-input", Delivery: "queue", Model: "parked-model",
					Message: llm.Message{Role: "user", Content: "Keep this pending input"},
				})
			}
			if _, err := AllocateChat(ctx, database, nil, nil); err != nil {
				t.Fatalf("allocate with parked work: %v", err)
			}
			owned, err := OwnsChat(ctx, database, 1)
			if err != nil || !owned {
				t.Fatalf("parked chat preserved = %t, %v", owned, err)
			}
			if pendingState {
				if err := SetCompletionState(ctx, database, 1, CompletionState{}); err != nil {
					t.Fatal(err)
				}
			} else {
				items, err := GetInbox(ctx, database, 1)
				if err != nil || len(items) != 1 || items[0].Message.Content != item.Message.Content {
					t.Fatalf("parked inbox = %#v, %v", items, err)
				}
				if err := CancelInboxPrompt(ctx, database, 1, item.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := AllocateChat(ctx, database, nil, nil); err != nil {
				t.Fatalf("allocate after parked work cleared: %v", err)
			}
			owned, err = OwnsChat(ctx, database, 1)
			if err != nil || owned {
				t.Fatalf("abandoned empty chat preserved = %t, %v; want false", owned, err)
			}
		})
	}
}

func TestInboxEnqueuePreservesPendingOrderAndSnapshots(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)

	image := llm.UserAttachment{Filename: "image.png", MediaType: "image/png", Width: 2, Height: 3, Data: []byte{1, 2, 3}}
	pdf := llm.UserAttachment{Filename: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.7\n")}
	inputs := []InboxItem{
		{
			SubmissionID: "first",
			Delivery:     "queue",
			Model:        "model-a",
			Tools:        []string{"webfetch", "git"},
			AppendIDs:    []string{"link-check", "research"},
			Message: llm.Message{
				Role:              "user",
				Content:           "first prompt",
				PromptAppendTexts: []string{"append one", "append two"},
				Attachments:       []llm.UserAttachment{image},
			},
		},
		{
			SubmissionID: "second",
			Delivery:     "steer",
			Model:        "model-b",
			Tools:        []string{"git"},
			AppendIDs:    []string{"research"},
			Message:      llm.Message{Role: "user", Content: "second prompt", Attachments: []llm.UserAttachment{pdf}},
		},
		{
			SubmissionID: "third",
			Delivery:     "queue",
			Model:        "",
			Message:      llm.Message{Role: "user", Content: "third prompt"},
		},
	}
	for _, input := range inputs {
		stored := enqueueInboxTestItem(t, database, ctx, 1, input)
		if stored.Status != "pending" || stored.ID <= 0 {
			t.Fatalf("stored item = %#v, want pending with positive ID", stored)
		}
	}

	pending, err := GetInbox(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetInbox() error: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("GetInbox() length = %d, want 3", len(pending))
	}
	for index, item := range pending {
		input := inputs[index]
		if item.ID != int64(index+1) || item.SubmissionID != input.SubmissionID {
			t.Errorf("pending[%d] identity = %d %q, want %d %q", index, item.ID, item.SubmissionID, index+1, input.SubmissionID)
		}
		if item.Delivery != input.Delivery || item.Model != input.Model || item.Status != "pending" {
			t.Errorf("pending[%d] fields = %#v, want delivery %q model %q", index, item, input.Delivery, input.Model)
		}
		if !slices.Equal(item.Tools, input.Tools) || !slices.Equal(item.AppendIDs, input.AppendIDs) {
			t.Errorf("pending[%d] selections = %v %v, want %v %v", index, item.Tools, item.AppendIDs, input.Tools, input.AppendIDs)
		}
		if item.Message.Content != input.Message.Content || !slices.Equal(item.Message.PromptAppendTexts, input.Message.PromptAppendTexts) {
			t.Errorf("pending[%d] message = %#v, want content %q", index, item.Message, input.Message.Content)
		}
		if !slices.EqualFunc(item.Message.Attachments, input.Message.Attachments, func(a, b llm.UserAttachment) bool {
			return a.Filename == b.Filename && a.MediaType == b.MediaType && a.Width == b.Width && a.Height == b.Height && slices.Equal(a.Data, b.Data)
		}) {
			t.Errorf("pending[%d] attachments = %#v, want %#v", index, item.Message.Attachments, input.Message.Attachments)
		}
	}

	duplicate := enqueueInboxTestItem(t, database, ctx, 1, inputs[0])
	if duplicate.ID != pending[0].ID || duplicate.Status != "pending" || !slices.Equal(duplicate.Tools, inputs[0].Tools) {
		t.Fatalf("duplicate receipt = %#v, want original %#v", duplicate, pending[0])
	}
	pending, err = GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 3 {
		t.Fatalf("GetInbox() after duplicate = %d, %v, want 3", len(pending), err)
	}
}

func TestInboxPendingLimitCancelAndModeChange(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)

	for index := 0; index < maxPendingInboxItems; index++ {
		enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
			SubmissionID: "pending-" + string(rune('a'+index%26)) + string(rune('a'+index/26)),
			Delivery:     "queue",
			Model:        "model",
			Message:      llm.Message{Role: "user", Content: "prompt"},
		})
	}
	_, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "overflow",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "prompt"},
	})
	if !errors.Is(err, ErrInboxFull) {
		t.Fatalf("EnqueuePrompt(overflow) error = %v, want ErrInboxFull", err)
	}

	if err := SetInboxDelivery(ctx, database, 1, 1, "steer"); err != nil {
		t.Fatalf("SetInboxDelivery() error: %v", err)
	}
	pending, err := GetInbox(ctx, database, 1)
	if err != nil || len(pending) != maxPendingInboxItems || pending[0].Delivery != "steer" {
		t.Fatalf("GetInbox() after mode change = %#v, %v", pending, err)
	}

	if err := CancelInboxPrompt(ctx, database, 1, 1); err != nil {
		t.Fatalf("CancelInboxPrompt() error: %v", err)
	}
	pending, err = GetInbox(ctx, database, 1)
	if err != nil || len(pending) != maxPendingInboxItems-1 || pending[0].ID == 1 {
		t.Fatalf("GetInbox() after cancel = %#v, %v", pending, err)
	}
	if err := CancelInboxPrompt(ctx, database, 1, 1); !errors.Is(err, ErrInboxConflict) {
		t.Fatalf("second CancelInboxPrompt() error = %v, want ErrInboxConflict", err)
	}
	if err := SetInboxDelivery(ctx, database, 1, 1, "queue"); !errors.Is(err, ErrInboxConflict) {
		t.Fatalf("SetInboxDelivery(cancelled) error = %v, want ErrInboxConflict", err)
	}

	replacement := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "replacement",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "prompt"},
	})
	if replacement.ID <= maxPendingInboxItems {
		t.Fatalf("replacement ID = %d, want monotonic ID beyond cancelled item", replacement.ID)
	}

	receipt := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "pending-aa",
		Delivery:     "queue",
		Model:        "ignored",
		Message:      llm.Message{Role: "user", Content: "ignored"},
		Status:       "cancelled",
	})
	if receipt.ID != 1 || receipt.Status != "cancelled" || receipt.Delivery != "steer" || receipt.Message.Content != "prompt" {
		t.Fatalf("cancelled receipt = %#v, want retained cancelled receipt", receipt)
	}
	if len(receipt.Tools) != 0 || len(receipt.AppendIDs) != 0 || len(receipt.Message.Attachments) != 0 {
		t.Fatalf("cancelled receipt payload = %#v, want discarded children", receipt)
	}
	if _, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "invalid",
		Delivery:     "later",
		Model:        "model",
		Message:      llm.Message{Role: "user"},
	}); err == nil {
		t.Fatal("EnqueuePrompt(invalid delivery) error = nil")
	}
	if _, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "assistant",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "assistant"},
	}); err == nil {
		t.Fatal("EnqueuePrompt(assistant message) error = nil")
	}
}

func TestInboxCancelDiscardsPayloadAndKeepsReceipt(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)
	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "cancel-me",
		Delivery:     "steer",
		Model:        "model",
		Tools:        []string{"git"},
		AppendIDs:    []string{"research"},
		Message: llm.Message{
			Role: "user", Content: "cancel content",
			PromptAppendTexts: []string{"append text"},
			Attachments:       []llm.UserAttachment{{Filename: "a.png", MediaType: "image/png", Width: 1, Height: 1, Data: []byte{1}}},
		},
	})
	if err := CancelInboxPrompt(ctx, database, 1, item.ID); err != nil {
		t.Fatalf("CancelInboxPrompt() error: %v", err)
	}
	for _, table := range []string{"inbox_tools", "inbox_prompt_appends", "inbox_prompt_append_texts", "inbox_attachments"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE inbox_id = ?`, item.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s rows after cancel = %d, want 0", table, count)
		}
	}
	receipt := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "cancel-me",
		Delivery:     "queue",
		Model:        "ignored",
		Message:      llm.Message{Role: "user", Content: "ignored"},
		Status:       "cancelled",
	})
	if receipt.ID != item.ID || receipt.Status != "cancelled" || receipt.Message.Content != "cancel content" {
		t.Fatalf("cancelled receipt = %#v, want retained base receipt", receipt)
	}
	if len(receipt.Tools) != 0 || len(receipt.AppendIDs) != 0 || len(receipt.Message.PromptAppendTexts) != 0 || len(receipt.Message.Attachments) != 0 {
		t.Fatalf("cancelled receipt retained discarded payload: %#v", receipt)
	}
}

func TestCheckpointCompletionPrioritizesSteersAndKeepsGeneratedOrder(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)
	if err := replaceChatOptions(ctx, database, 1, []string{"original-tool"}, []string{"link-check"}); err != nil {
		t.Fatalf("store initial chat options: %v", err)
	}

	queueOne := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queue-one",
		Delivery:     "queue",
		Model:        "queue-model",
		Tools:        []string{"queue-tool"},
		AppendIDs:    []string{"research"},
		Message:      llm.Message{Role: "user", Content: "queued one"},
	})
	steerOne := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer-one",
		Delivery:     "steer",
		Model:        "steer-model",
		Tools:        []string{"steer-tool"},
		AppendIDs:    []string{"research"},
		Message: llm.Message{
			Role:              "user",
			Content:           "steer one",
			PromptAppendTexts: []string{"steer snapshot"},
			Attachments:       []llm.UserAttachment{{Filename: "steer.png", MediaType: "image/png", Width: 1, Height: 1, Data: []byte{7}}},
		},
	})
	queueTwo := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queue-two",
		Delivery:     "queue",
		Model:        "queue-model",
		Tools:        []string{"queue-tool"},
		AppendIDs:    []string{"research"},
		Message:      llm.Message{Role: "user", Content: "queued two"},
	})
	steerTwo := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer-two",
		Delivery:     "steer",
		Model:        "steer-model",
		Tools:        []string{"later-tool"},
		AppendIDs:    []string{"link-check", "research"},
		Message:      llm.Message{Role: "user", Content: "steer two"},
	})

	metadata, err := llm.NewResponsesProviderMetadata([]json.RawMessage{
		json.RawMessage(`{"id":"msg_1","type":"message","role":"assistant"}`),
	})
	if err != nil {
		t.Fatalf("build provider metadata: %v", err)
	}
	generated := []llm.Message{
		{
			Role:    "assistant",
			Content: "calling a tool",
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function",
				Function: llm.FunctionCall{Name: "webfetch", Arguments: `{"url":"https://example.com"}`},
			}},
			ProviderMetadata: metadata,
		},
		{Role: "tool", Content: "tool output", ToolCallID: "call_1"},
	}
	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, generated, "steer")
	if err != nil {
		t.Fatalf("CheckpointCompletion() error: %v", err)
	}

	if len(result.Appended) != 2 || len(result.Delivered) != 2 {
		t.Fatalf("checkpoint result = %#v, want two appended and two delivered", result)
	}
	for index, message := range result.Appended {
		if message.ID <= 0 || message.Content != generated[index].Content {
			t.Errorf("appended[%d] = %#v, want persisted generated message", index, message)
		}
	}
	if !slices.Equal([]int64{result.Delivered[0].ID, result.Delivered[1].ID}, []int64{steerOne.ID, steerTwo.ID}) {
		t.Errorf("delivered IDs = %d %d, want %d %d", result.Delivered[0].ID, result.Delivered[1].ID, steerOne.ID, steerTwo.ID)
	}
	for index, item := range result.Delivered {
		want := []InboxItem{steerOne, steerTwo}[index]
		if item.Status != "delivered" || item.Message.ID <= 0 || item.Message.Content != want.Message.Content {
			t.Errorf("delivered[%d] = %#v, want persisted %q", index, item, want.Message.Content)
		}
	}

	messages := result.Snapshot.Messages
	if len(messages) != 5 {
		t.Fatalf("snapshot message count = %d, want 5: %#v", len(messages), messages)
	}
	gotRoles := []string{messages[0].Role, messages[1].Role, messages[2].Role, messages[3].Role, messages[4].Role}
	if !slices.Equal(gotRoles, []string{"user", "assistant", "tool", "user", "user"}) {
		t.Fatalf("snapshot roles = %v", gotRoles)
	}
	if messages[1].ID != result.Appended[0].ID || messages[2].ID != result.Appended[1].ID {
		t.Errorf("snapshot generated IDs = %d %d, want %d %d", messages[1].ID, messages[2].ID, result.Appended[0].ID, result.Appended[1].ID)
	}
	if len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].ID != "call_1" || messages[1].ToolCalls[0].Function.Arguments != `{"url":"https://example.com"}` {
		t.Errorf("tool call round trip = %#v", messages[1].ToolCalls)
	}
	if outputs := messages[1].ProviderMetadata.ResponsesOutput(); len(outputs) != 1 || !strings.Contains(string(outputs[0]), "msg_1") {
		t.Errorf("provider metadata round trip = %#v", outputs)
	}
	if messages[2].ToolCallID != "call_1" || messages[2].Content != "tool output" {
		t.Errorf("tool result round trip = %#v", messages[2])
	}
	if messages[3].Content != "steer one" || messages[4].Content != "steer two" {
		t.Errorf("delivered user order = %q %q", messages[3].Content, messages[4].Content)
	}
	if messages[3].ID != result.Delivered[0].Message.ID || messages[4].ID != result.Delivered[1].Message.ID {
		t.Errorf("snapshot delivered IDs = %d %d, want %d %d", messages[3].ID, messages[4].ID, result.Delivered[0].Message.ID, result.Delivered[1].Message.ID)
	}
	if !slices.Equal(messages[3].PromptAppendTexts, []string{"steer snapshot"}) || len(messages[3].Attachments) != 1 || !slices.Equal(messages[3].Attachments[0].Data, []byte{7}) {
		t.Errorf("delivered attachment/append snapshot = %#v", messages[3])
	}

	pending, err := GetInbox(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetInbox() after checkpoint: %v", err)
	}
	if len(pending) != 2 || pending[0].ID != queueOne.ID || pending[1].ID != queueTwo.ID {
		t.Fatalf("pending after steer checkpoint = %#v, want queue one and queue two", pending)
	}
	tools, err := GetChatTools(ctx, database, 1)
	if err != nil || !slices.Equal(tools, []string{"original-tool"}) {
		t.Fatalf("chat tools after steering = %v, %v, want unchanged original tool", tools, err)
	}
	appendIDs, err := GetChatPromptAppendIDs(ctx, database, 1)
	if err != nil || !slices.Equal(appendIDs, []string{"link-check", "research"}) {
		t.Fatalf("chat appends after steering = %v, %v, want last steering selection", appendIDs, err)
	}

	receipt := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: steerOne.SubmissionID,
		Delivery:     "steer",
		Model:        "ignored",
		Message:      llm.Message{Role: "user", Content: "ignored"},
		Status:       "delivered",
	})
	if receipt.ID != steerOne.ID || receipt.Status != "delivered" || receipt.Message.ID != result.Delivered[0].Message.ID {
		t.Fatalf("delivered receipt = %#v, want original delivered item", receipt)
	}
	if err := CancelInboxPrompt(ctx, database, 1, steerOne.ID); !errors.Is(err, ErrInboxConflict) {
		t.Fatalf("cancel delivered item error = %v, want ErrInboxConflict", err)
	}
	if err := SetInboxDelivery(ctx, database, 1, steerOne.ID, "queue"); !errors.Is(err, ErrInboxConflict) {
		t.Fatalf("change delivered item error = %v, want ErrInboxConflict", err)
	}
}

func TestCheckpointCompletionPromotesInputsOneQueueAtATime(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)
	if err := replaceChatOptions(ctx, database, 1, []string{"original-tool"}, []string{"link-check"}); err != nil {
		t.Fatalf("store initial chat options: %v", err)
	}

	queueOne := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queue-one",
		Delivery:     "queue",
		Model:        "queue-model",
		Tools:        []string{"queue-one-tool"},
		AppendIDs:    []string{"research"},
		Message:      llm.Message{Role: "user", Content: "queue one"},
	})
	queueTwo := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queue-two",
		Delivery:     "queue",
		Model:        "queue-model",
		Tools:        []string{"queue-two-tool"},
		AppendIDs:    []string{"link-check"},
		Message:      llm.Message{Role: "user", Content: "queue two"},
	})

	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "first answer"}}, "input")
	if err != nil {
		t.Fatalf("first input checkpoint error: %v", err)
	}
	if len(result.Delivered) != 1 || result.Delivered[0].ID != queueOne.ID || result.Delivered[0].Message.Content != "queue one" {
		t.Fatalf("first input delivery = %#v, want only queue one", result.Delivered)
	}
	if len(result.Snapshot.Messages) != 3 || result.Snapshot.Messages[2].Content != "queue one" {
		t.Fatalf("first input snapshot = %#v", result.Snapshot.Messages)
	}
	tools, err := GetChatTools(ctx, database, 1)
	if err != nil || !slices.Equal(tools, queueOne.Tools) {
		t.Fatalf("chat tools after queue delivery = %v, %v, want %v", tools, err, queueOne.Tools)
	}
	appendIDs, err := GetChatPromptAppendIDs(ctx, database, 1)
	if err != nil || !slices.Equal(appendIDs, queueOne.AppendIDs) {
		t.Fatalf("chat appends after queue delivery = %v, %v, want %v", appendIDs, err, queueOne.AppendIDs)
	}
	pending, err := GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != queueTwo.ID {
		t.Fatalf("pending after first input = %#v, %v, want queue two", pending, err)
	}

	steerOne := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer-one",
		Delivery:     "steer",
		Model:        "steer-model",
		Tools:        []string{"steer-tool"},
		AppendIDs:    []string{"research"},
		Message:      llm.Message{Role: "user", Content: "steer one"},
	})
	steerTwo := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer-two",
		Delivery:     "steer",
		Model:        "steer-model",
		Tools:        []string{"steer-two-tool"},
		AppendIDs:    []string{"link-check"},
		Message:      llm.Message{Role: "user", Content: "steer two"},
	})
	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() before steers: %v", err)
	}
	result, err = CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "second answer"}}, "input")
	if err != nil {
		t.Fatalf("steer input checkpoint error: %v", err)
	}
	if len(result.Delivered) != 2 || result.Delivered[0].ID != steerOne.ID || result.Delivered[1].ID != steerTwo.ID {
		t.Fatalf("input delivery with steers = %#v, want both steers", result.Delivered)
	}
	pending, err = GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != queueTwo.ID {
		t.Fatalf("pending after steer input = %#v, %v, want queue two still pending", pending, err)
	}
	tools, err = GetChatTools(ctx, database, 1)
	if err != nil || !slices.Equal(tools, queueOne.Tools) {
		t.Fatalf("chat tools after steering = %v, %v, want prior queue selection", tools, err)
	}

	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() before final queue: %v", err)
	}
	result, err = CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "third answer"}}, "input")
	if err != nil {
		t.Fatalf("final queue checkpoint error: %v", err)
	}
	if len(result.Delivered) != 1 || result.Delivered[0].ID != queueTwo.ID {
		t.Fatalf("final input delivery = %#v, want queue two", result.Delivered)
	}
	tools, err = GetChatTools(ctx, database, 1)
	if err != nil || !slices.Equal(tools, queueTwo.Tools) {
		t.Fatalf("chat tools after second queue = %v, %v, want %v", tools, err, queueTwo.Tools)
	}
	pending, err = GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after final queue = %#v, %v, want empty", pending, err)
	}
}

func TestCheckpointCompletionRollsBackAndPreservesPendingItems(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)

	queue := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queue",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "queued"},
	})
	steer := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer",
		Delivery:     "steer",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "steering"},
	})
	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	if _, err := database.Exec(`UPDATE messages SET content = 'edited' WHERE chat_id = 1`); err != nil {
		t.Fatalf("edit stored message: %v", err)
	}
	_, err = CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "answer"}}, "input")
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale checkpoint error = %v, want ErrConversationConflict", err)
	}
	pending, err := GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 2 || pending[0].ID != queue.ID || pending[1].ID != steer.ID {
		t.Fatalf("pending after stale checkpoint = %#v, %v, want both pending", pending, err)
	}

	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() after conflict: %v", err)
	}
	_, err = CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{
		{Role: "assistant", Content: "partially written"},
		{Role: "invalid-role", Content: "fails"},
	}, "input")
	if err == nil {
		t.Fatal("checkpoint with invalid generated role error = nil")
	}
	messages, err := GetMessages(ctx, database, 1)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages after failed checkpoint = %#v, %v, want only original user message", messages, err)
	}
	pending, err = GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending after failed checkpoint = %#v, %v, want both pending", pending, err)
	}

	otherChat := int64(2)
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (2)`); err != nil {
		t.Fatalf("insert second chat: %v", err)
	}
	snapshot, err = GetMessageSnapshot(ctx, database, otherChat)
	if err != nil {
		t.Fatalf("GetMessageSnapshot(second chat) error: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 1, snapshot, nil, ""); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("checkpoint with foreign snapshot error = %v, want ErrConversationConflict", err)
	}
}

func TestCheckpointWithoutPromotionKeepsUndoHistory(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'first'),
			(1, 1, 'assistant', 'first answer'),
			(1, 2, 'user', 'second'),
			(1, 3, 'assistant', 'second answer');
	`); err != nil {
		t.Fatalf("insert chat history: %v", err)
	}
	if _, err := UndoLatestTurn(ctx, database, 1); err != nil {
		t.Fatalf("UndoLatestTurn() error: %v", err)
	}
	queue := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "waiting",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "waiting"},
	})

	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, nil, "")
	if err != nil {
		t.Fatalf("no-op CheckpointCompletion() error: %v", err)
	}
	if len(result.Appended) != 0 || len(result.Delivered) != 0 || len(result.Snapshot.Messages) != 2 {
		t.Fatalf("no-op checkpoint result = %#v", result)
	}
	pending, err := GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != queue.ID {
		t.Fatalf("pending after no-op checkpoint = %#v, %v", pending, err)
	}
	redone, err := RedoLatestTurn(ctx, database, 1)
	if err != nil {
		t.Fatalf("RedoLatestTurn() after no-op checkpoint error: %v", err)
	}
	if len(redone) != 4 || redone[2].Content != "second" {
		t.Fatalf("redo after no-op checkpoint = %#v, want hidden history preserved", redone)
	}
}

func TestCheckpointCompletionUpdatesPendingStateAtomically(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)

	if err := SetCompletionState(ctx, database, 1, CompletionState{
		Pending: true,
		Model:   "active-model",
		Tools:   []string{"webfetch", "git"},
	}); err != nil {
		t.Fatalf("SetCompletionState() error: %v", err)
	}
	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "final answer"}}, ""); err != nil {
		t.Fatalf("final assistant checkpoint error: %v", err)
	}
	state, err := GetCompletionState(ctx, database, 1)
	if err != nil || state.Pending || state.Model != "active-model" || !slices.Equal(state.Tools, []string{"webfetch", "git"}) {
		t.Fatalf("state after final assistant = %#v, %v, want completed state with preserved model/tools", state, err)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "active-model", Tools: []string{"webfetch", "git"}}); err != nil {
		t.Fatalf("reset completion state: %v", err)
	}
	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() before tool turn: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "webfetch", Arguments: `{}`},
		}}},
		{Role: "tool", Content: "result", ToolCallID: "call_1"},
	}, ""); err != nil {
		t.Fatalf("tool checkpoint error: %v", err)
	}
	state, err = GetCompletionState(ctx, database, 1)
	if err != nil || !state.Pending || state.Model != "active-model" || !slices.Equal(state.Tools, []string{"webfetch", "git"}) {
		t.Fatalf("state after tool result = %#v, %v, want pending with preserved model/tools", state, err)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: false, Model: "queue-model", Tools: []string{"git"}}); err != nil {
		t.Fatalf("set completed queue state: %v", err)
	}
	enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queued",
		Delivery:     "queue",
		Model:        "queue-model",
		Tools:        []string{"queue-tool"},
		Message:      llm.Message{Role: "user", Content: "queued"},
	})
	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() before queue promotion: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "answer before queue"}}, "input")
	if err != nil || len(result.Delivered) != 1 {
		t.Fatalf("queue checkpoint = %#v, %v, want one delivery", result, err)
	}
	state, err = GetCompletionState(ctx, database, 1)
	if err != nil || !state.Pending || state.Model != "queue-model" || !slices.Equal(state.Tools, []string{"queue-tool"}) {
		t.Fatalf("state after queued input = %#v, %v, want captured queue selection", state, err)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "kept", Tools: []string{"kept-tool"}}); err != nil {
		t.Fatalf("set no-op state: %v", err)
	}
	snapshot, err = GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() before no-op checkpoint: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 1, snapshot, nil, ""); err != nil {
		t.Fatalf("no-op checkpoint error: %v", err)
	}
	state, err = GetCompletionState(ctx, database, 1)
	if err != nil || !state.Pending || state.Model != "kept" || !slices.Equal(state.Tools, []string{"kept-tool"}) {
		t.Fatalf("state after no-op checkpoint = %#v, %v, want unchanged pending state", state, err)
	}

	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (2)`); err != nil {
		t.Fatalf("insert second chat: %v", err)
	}
	snapshot, err = GetMessageSnapshot(ctx, database, 2)
	if err != nil {
		t.Fatalf("GetMessageSnapshot(second chat) error: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 2, snapshot, []llm.Message{{Role: "assistant", Content: "final"}}, ""); err != nil {
		t.Fatalf("checkpoint without stored state error: %v", err)
	}
	state, err = GetCompletionState(ctx, database, 2)
	if err != nil || state.Pending || state.Model != "" || len(state.Tools) != 0 {
		t.Fatalf("created completion state = %#v, %v, want completed empty-model state", state, err)
	}
}

func TestCheckpointQueuePromotionPersistsCapturedStateAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	database := openMessagesTestDatabase(t, path)
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)
	if err := SetCompletionState(ctx, database, 1, CompletionState{
		Pending: true,
		Model:   "previous-model",
		Tools:   []string{"previous-tool"},
	}); err != nil {
		t.Fatalf("SetCompletionState(previous turn) error: %v", err)
	}
	item := enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "queued-next-turn",
		Delivery:     "queue",
		Model:        "queued-model",
		Tools:        []string{"queued-tool", "second-tool"},
		AppendIDs:    []string{"research"},
		Message:      llm.Message{Role: "user", Content: "next turn"},
	})
	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	result, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "previous answer"}}, "input")
	if err != nil {
		t.Fatalf("CheckpointCompletion(queue promotion) error: %v", err)
	}
	if len(result.Delivered) != 1 || result.Delivered[0].ID != item.ID {
		t.Fatalf("queue promotion delivered = %#v, want %d", result.Delivered, item.ID)
	}

	// No separate SetCompletionState call: a crash here must still leave the
	// queued prompt's captured selection durable.
	reopened := openMessagesTestConnection(t, path)
	state, err := GetCompletionState(ctx, reopened, 1)
	if err != nil {
		t.Fatalf("GetCompletionState(reopened) error: %v", err)
	}
	if !state.Pending || state.Model != "queued-model" || !slices.Equal(state.Tools, []string{"queued-tool", "second-tool"}) {
		t.Fatalf("completion state after queue promotion = %#v, want captured queued selection", state)
	}
}

func TestCheckpointCompletionWithStatePersistsAtomicState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	database := openMessagesTestDatabase(t, path)
	ctx := inboxTestContext()
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1), (2), (3);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'idle'),
			(2, 0, 'user', 'queued'),
			(3, 0, 'user', 'settle');
	`); err != nil {
		t.Fatalf("insert checkpoint chats: %v", err)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "old-model", Tools: []string{"old-tool"}}); err != nil {
		t.Fatalf("seed idle completion state: %v", err)
	}
	enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "idle-steer",
		Delivery:     "steer",
		Model:        "item-model",
		Tools:        []string{"item-tool"},
		Message:      llm.Message{Role: "user", Content: "idle steering"},
	})
	idleSnapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot(idle) error: %v", err)
	}
	idleResult, err := CheckpointCompletionWithState(ctx, database, 1, idleSnapshot, nil, "input", CompletionState{
		Pending: false,
		Model:   "  idle-model  ",
		Tools:   []string{"idle-tool", "idle-tool"},
	})
	if err != nil {
		t.Fatalf("CheckpointCompletionWithState(idle steering) error: %v", err)
	}
	if len(idleResult.Delivered) != 1 || idleResult.Delivered[0].Message.Content != "idle steering" {
		t.Fatalf("idle steering delivered = %#v", idleResult.Delivered)
	}

	enqueueInboxTestItem(t, database, ctx, 2, InboxItem{
		SubmissionID: "queued-turn",
		Delivery:     "queue",
		Model:        "queued-model",
		Tools:        []string{"queued-tool"},
		Message:      llm.Message{Role: "user", Content: "queued turn"},
	})
	queuedSnapshot, err := GetMessageSnapshot(ctx, database, 2)
	if err != nil {
		t.Fatalf("GetMessageSnapshot(queued) error: %v", err)
	}
	queuedResult, err := CheckpointCompletionWithState(ctx, database, 2, queuedSnapshot, nil, "input", CompletionState{
		Pending: false,
		Model:   "override-model",
		Tools:   []string{"override-tool"},
	})
	if err != nil {
		t.Fatalf("CheckpointCompletionWithState(queue) error: %v", err)
	}
	if len(queuedResult.Delivered) != 1 || queuedResult.Delivered[0].Delivery != "queue" {
		t.Fatalf("queue delivery = %#v", queuedResult.Delivered)
	}

	settleSnapshot, err := GetMessageSnapshot(ctx, database, 3)
	if err != nil {
		t.Fatalf("GetMessageSnapshot(settle) error: %v", err)
	}
	settledResult, err := CheckpointCompletionWithState(ctx, database, 3, settleSnapshot, []llm.Message{{Role: "assistant", Content: "done"}}, "", CompletionState{
		Pending: true,
		Model:   "settled-model",
		Tools:   []string{"settled-tool"},
	})
	if err != nil {
		t.Fatalf("CheckpointCompletionWithState(settlement) error: %v", err)
	}
	if len(settledResult.Appended) != 1 || settledResult.Appended[0].Content != "done" {
		t.Fatalf("settlement appended = %#v", settledResult.Appended)
	}

	reopened := openMessagesTestConnection(t, path)
	idleState, err := GetCompletionState(ctx, reopened, 1)
	if err != nil || !idleState.Pending || idleState.Model != "idle-model" || !slices.Equal(idleState.Tools, []string{"idle-tool"}) {
		t.Fatalf("idle steering state = %#v, %v, want override with delivery forcing pending", idleState, err)
	}
	queuedState, err := GetCompletionState(ctx, reopened, 2)
	if err != nil || !queuedState.Pending || queuedState.Model != "queued-model" || !slices.Equal(queuedState.Tools, []string{"queued-tool"}) {
		t.Fatalf("queued state = %#v, %v, want captured queue selection", queuedState, err)
	}
	settledState, err := GetCompletionState(ctx, reopened, 3)
	if err != nil || settledState.Pending || settledState.Model != "settled-model" || !slices.Equal(settledState.Tools, []string{"settled-tool"}) {
		t.Fatalf("settled state = %#v, %v, want supplied completed state", settledState, err)
	}
}

func TestCheckpointSteeringPreservesActiveCompletionState(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := inboxTestContext()
	insertMessagesTestUser(t, database)
	if err := SetCompletionState(ctx, database, 1, CompletionState{
		Pending: true,
		Model:   "active-model",
		Tools:   []string{"active-tool"},
	}); err != nil {
		t.Fatalf("SetCompletionState() error: %v", err)
	}
	enqueueInboxTestItem(t, database, ctx, 1, InboxItem{
		SubmissionID: "steer",
		Delivery:     "steer",
		Model:        "steer-item-model",
		Tools:        []string{"steer-item-tool"},
		Message:      llm.Message{Role: "user", Content: "steering"},
	})
	snapshot, err := GetMessageSnapshot(ctx, database, 1)
	if err != nil {
		t.Fatalf("GetMessageSnapshot() error: %v", err)
	}
	if _, err := CheckpointCompletion(ctx, database, 1, snapshot, []llm.Message{{Role: "assistant", Content: "answer"}}, "steer"); err != nil {
		t.Fatalf("CheckpointCompletion(steering) error: %v", err)
	}
	state, err := GetCompletionState(ctx, database, 1)
	if err != nil || !state.Pending || state.Model != "active-model" || !slices.Equal(state.Tools, []string{"active-tool"}) {
		t.Fatalf("state after steering = %#v, %v, want active selection preserved", state, err)
	}
}

func TestInboxAndCompletionStateAreAccountScoped(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")
	aliceChat, err := AllocateChat(aliceCtx, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate Alice chat: %v", err)
	}
	bobChat, err := AllocateChat(bobCtx, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate Bob chat: %v", err)
	}

	aliceItem, err := EnqueuePrompt(aliceCtx, database, aliceChat, InboxItem{
		SubmissionID: "shared-id",
		Delivery:     "queue",
		Model:        "alice-model",
		Tools:        []string{"alice-tool"},
		Message:      llm.Message{Role: "user", Content: "alice"},
	})
	if err != nil {
		t.Fatalf("enqueue Alice prompt: %v", err)
	}
	bobItem, err := EnqueuePrompt(bobCtx, database, bobChat, InboxItem{
		SubmissionID: "shared-id",
		Delivery:     "queue",
		Model:        "bob-model",
		Tools:        []string{"bob-tool"},
		Message:      llm.Message{Role: "user", Content: "bob"},
	})
	if err != nil {
		t.Fatalf("enqueue Bob prompt with same submission ID: %v", err)
	}
	if aliceItem.ID == bobItem.ID {
		t.Fatalf("cross-chat submission IDs shared receipt ID %d", aliceItem.ID)
	}

	if _, err := GetInbox(bobCtx, database, aliceChat); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob GetInbox(Alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := EnqueuePrompt(bobCtx, database, aliceChat, InboxItem{
		SubmissionID: "intruder", Delivery: "queue", Model: "model", Message: llm.Message{Role: "user"},
	}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob EnqueuePrompt(Alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := CancelInboxPrompt(bobCtx, database, aliceChat, aliceItem.ID); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob CancelInboxPrompt(Alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := SetInboxDelivery(bobCtx, database, aliceChat, aliceItem.ID, "steer"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob SetInboxDelivery(Alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := SetCompletionState(bobCtx, database, aliceChat, CompletionState{Pending: true, Model: "bob"}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob SetCompletionState(Alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := GetCompletionState(bobCtx, database, aliceChat); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob GetCompletionState(Alice chat) error = %v, want ErrChatNotFound", err)
	}

	alicePending, err := GetInbox(aliceCtx, database, aliceChat)
	if err != nil || len(alicePending) != 1 || alicePending[0].Message.Content != "alice" {
		t.Fatalf("Alice inbox = %#v, %v", alicePending, err)
	}
	bobPending, err := GetInbox(bobCtx, database, bobChat)
	if err != nil || len(bobPending) != 1 || bobPending[0].Message.Content != "bob" {
		t.Fatalf("Bob inbox = %#v, %v", bobPending, err)
	}

	snapshot, err := GetMessageSnapshot(aliceCtx, database, aliceChat)
	if err != nil {
		t.Fatalf("Alice message snapshot: %v", err)
	}
	if _, err := CheckpointCompletion(bobCtx, database, aliceChat, snapshot, nil, "input"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("Bob checkpoint(Alice chat) error = %v, want ErrChatNotFound", err)
	}
}

func TestCompletionStatePersistsAndNormalizesTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	database := openMessagesTestDatabase(t, path)
	ctx := inboxTestContext()
	if _, err := database.Exec(`INSERT INTO chats (id) VALUES (1)`); err != nil {
		t.Fatalf("insert chat: %v", err)
	}

	state, err := GetCompletionState(ctx, database, 1)
	if err != nil || state.Pending || state.Model != "" || len(state.Tools) != 0 {
		t.Fatalf("initial completion state = %#v, %v, want zero", state, err)
	}
	if err := SetCompletionState(ctx, database, 1, CompletionState{
		Pending: true,
		Model:   "  model with spaces  ",
		Tools:   []string{" webfetch", "git", "webfetch", "  ", "git"},
	}); err != nil {
		t.Fatalf("SetCompletionState() error: %v", err)
	}

	reopened := openMessagesTestConnection(t, path)
	state, err = GetCompletionState(ctx, reopened, 1)
	if err != nil {
		t.Fatalf("GetCompletionState(reopened) error: %v", err)
	}
	if !state.Pending || state.Model != "model with spaces" || !slices.Equal(state.Tools, []string{"webfetch", "git"}) {
		t.Fatalf("reopened completion state = %#v, want normalized pending state", state)
	}

	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: false, Model: "", Tools: nil}); err != nil {
		t.Fatalf("clear completion state: %v", err)
	}
	state, err = GetCompletionState(ctx, database, 1)
	if err != nil || state.Pending || state.Model != "" || len(state.Tools) != 0 {
		t.Fatalf("cleared completion state = %#v, %v, want zero", state, err)
	}
	if err := SetCompletionState(ctx, database, 999, CompletionState{Pending: true}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("SetCompletionState(missing chat) error = %v, want ErrChatNotFound", err)
	}
}

func TestInboxSchemaFilesMatch(t *testing.T) {
	start := strings.Index(messagesTestSchema, "CREATE TABLE IF NOT EXISTS inbox_items")
	if start < 0 {
		t.Fatal("schema.sql has no inbox definitions")
	}
	end := strings.Index(messagesTestSchema[start:], "CREATE TABLE IF NOT EXISTS models_dev_cache")
	if end < 0 {
		t.Fatal("schema.sql has no models.dev cache block")
	}
	embedded := strings.TrimSpace(messagesTestSchema[start : start+end])
	if embedded != strings.TrimSpace(inboxSchema) {
		t.Fatalf("schema.sql inbox block differs from inbox_schema.sql:\n%s\n---\n%s", embedded, inboxSchema)
	}
}

func TestMigrateInboxFromVersion25(t *testing.T) {
	fixture := messagesTestSchema
	index := strings.Index(fixture, "CREATE TABLE IF NOT EXISTS inbox_items")
	if index < 0 {
		t.Fatal("schema fixture has no inbox tables")
	}
	version25 := fixture[:index] + "PRAGMA user_version = 25;"
	version25 = strings.Replace(version25, "    system_prompt TEXT,\n", "", 1)
	version25 = strings.Replace(version25, "    compaction_prompt TEXT,\n", "", 1)

	database := openMessagesTestConnection(t, "")
	if _, err := database.Exec(version25); err != nil {
		t.Fatalf("initialize version 25 fixture: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO chats (id) VALUES (1);
		INSERT INTO messages (chat_id, position, role, content) VALUES (1, 0, 'user', 'before migration');
	`); err != nil {
		t.Fatalf("insert version 25 history: %v", err)
	}
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 25 {
		t.Fatalf("fixture version = %d, %v, want 25", version, err)
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin inbox migration: %v", err)
	}
	if err := MigrateInbox(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatalf("MigrateInbox() error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit inbox migration: %v", err)
	}
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 25 {
		t.Fatalf("version after MigrateInbox = %d, %v, want MigrateInbox to leave version recording to caller", version, err)
	}

	for _, table := range []string{"inbox_items", "inbox_tools", "inbox_prompt_appends", "inbox_prompt_append_texts", "inbox_attachments", "completion_state", "completion_state_tools"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s count = %d, want 1", table, count)
		}
	}

	messages, err := GetMessages(context.Background(), database, 1)
	if err != nil || len(messages) != 1 || messages[0].Content != "before migration" {
		t.Fatalf("history after migration = %#v, %v", messages, err)
	}
	ctx := inboxTestContext()
	item, err := EnqueuePrompt(ctx, database, 1, InboxItem{
		SubmissionID: "after-migration",
		Delivery:     "queue",
		Model:        "model",
		Message:      llm.Message{Role: "user", Content: "after"},
	})
	if err != nil || item.ID <= 0 {
		t.Fatalf("EnqueuePrompt() after migration = %#v, %v", item, err)
	}
	if err := SetCompletionState(ctx, database, 1, CompletionState{Pending: true, Model: "model", Tools: []string{"git"}}); err != nil {
		t.Fatalf("SetCompletionState() after migration: %v", err)
	}

	tx, err = database.Begin()
	if err != nil {
		t.Fatalf("begin repeated inbox migration: %v", err)
	}
	if err := MigrateInbox(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatalf("repeated MigrateInbox() error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit repeated inbox migration: %v", err)
	}
	pending, err := GetInbox(ctx, database, 1)
	if err != nil || len(pending) != 1 || pending[0].SubmissionID != "after-migration" {
		t.Fatalf("inbox after repeated migration = %#v, %v", pending, err)
	}
}
