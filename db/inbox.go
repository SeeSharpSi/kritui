package kritui_db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"seesharpsi/kritui/llm"
)

//go:embed inbox_schema.sql
var inboxSchema string

const (
	maxPendingInboxItems = 32
	maxInboxModelBytes   = 512
	maxInboxIDBytes      = 512

	inboxContentScope            = "inbox_items.content"
	inboxAppendTextScope         = "inbox_prompt_append_texts.text"
	inboxAttachmentFilenameScope = "inbox_attachments.filename"
	inboxAttachmentDataScope     = "inbox_attachments.data"
)

func inboxStorageRowID(inboxID int64) string {
	return strconv.FormatInt(inboxID, 10)
}

func inboxChildStorageRowID(inboxID int64, position int) string {
	return fmt.Sprintf("%d:%d", inboxID, position)
}

var (
	// ErrInboxFull means a chat already has the maximum number of pending
	// prompts waiting for delivery.
	ErrInboxFull = errors.New("inbox full")
	// ErrInboxConflict means an inbox mutation targeted a prompt that is no
	// longer pending in the chat.
	ErrInboxConflict = errors.New("inbox item is not pending")
)

// InboxItem is one durable prompt submission. Tools and AppendIDs are the
// selections snapshotted at submission time; Message is the user turn before
// prompt appends are expanded for a provider.
type InboxItem struct {
	ID           int64
	SubmissionID string
	Delivery     string
	Model        string
	Tools        []string
	AppendIDs    []string
	Message      llm.Message
	Status       string
}

// CheckpointResult describes the durable state committed after one model
// completion round.
type CheckpointResult struct {
	Snapshot  MessageSnapshot
	Appended  []llm.Message
	Delivered []InboxItem
}

// CompletionState records whether a chat still has an unfinished completion.
// It lets a restarted process offer a retry when the last durable message is a
// tool result.
type CompletionState struct {
	Pending bool
	Model   string
	Tools   []string
}

// MigrateInbox creates the version 26 inbox and completion-state tables. It is
// safe to run against a database where the tables already exist.
func MigrateInbox(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, inboxSchema); err != nil {
		return fmt.Errorf("migrate inbox schema: %w", err)
	}
	return nil
}

// EnqueuePrompt durably stores a pending prompt. A repeated submission ID
// returns the original receipt without inserting or replaying anything.
func EnqueuePrompt(ctx context.Context, db databaseExecutor, chatID int64, item InboxItem) (InboxItem, error) {
	submissionID := strings.TrimSpace(item.SubmissionID)
	if submissionID == "" {
		return InboxItem{}, errors.New("enqueue prompt: submission ID is required")
	}
	if len([]byte(submissionID)) > maxInboxIDBytes {
		return InboxItem{}, fmt.Errorf("enqueue prompt: submission ID exceeds %d bytes", maxInboxIDBytes)
	}

	var result InboxItem
	err := executeAtomically(ctx, db, func(executor databaseExecutor) error {
		// Reserve the writer lock before checking for an existing submission so
		// concurrent retries serialize on the first receipt. The receipt wins
		// even when a retry carries a different payload or status.
		if err := reserveChat(ctx, executor, chatID); err != nil {
			return err
		}
		queryer, err := inboxQueryer(executor)
		if err != nil {
			return err
		}
		existing, found, err := inboxItemBySubmission(ctx, queryer, chatID, submissionID)
		if err != nil {
			return err
		}
		if found {
			result = existing
			return nil
		}

		prepared, err := prepareInboxItem(item)
		if err != nil {
			return err
		}
		var pending int
		if err := executor.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM inbox_items
			WHERE chat_id = ? AND status = 'pending'
		`, chatID).Scan(&pending); err != nil {
			return fmt.Errorf("count pending inbox items: %w", err)
		}
		if pending >= maxPendingInboxItems {
			return ErrInboxFull
		}

		inserted, err := executor.ExecContext(ctx, `
			INSERT INTO inbox_items (chat_id, submission_id, delivery, model, status, content)
			VALUES (?, ?, ?, ?, 'pending', '')
		`, chatID, prepared.SubmissionID, prepared.Delivery, prepared.Model)
		if err != nil {
			return fmt.Errorf("insert inbox item: %w", err)
		}
		id, err := inserted.LastInsertId()
		if err != nil {
			return fmt.Errorf("get inbox item ID: %w", err)
		}
		sealedContent, err := EncryptStorageText(ctx, inboxContentScope, inboxStorageRowID(id), prepared.Message.Content)
		if err != nil {
			return fmt.Errorf("encrypt inbox content: %w", err)
		}
		if _, err := executor.ExecContext(ctx, `
			UPDATE inbox_items
			SET content = ?
			WHERE id = ?
		`, sealedContent, id); err != nil {
			return fmt.Errorf("store inbox content: %w", err)
		}
		if err := insertInboxChildren(ctx, executor, id, prepared); err != nil {
			return err
		}
		prepared.ID = id
		prepared.Status = "pending"
		result = prepared
		return nil
	})
	if err != nil {
		return InboxItem{}, fmt.Errorf("enqueue prompt: %w", err)
	}
	return result, nil
}

// GetInbox returns pending prompts for one chat in FIFO order.
func GetInbox(ctx context.Context, db databaseExecutor, chatID int64) ([]InboxItem, error) {
	if err := requireChatOwned(ctx, db, chatID); err != nil {
		return nil, fmt.Errorf("get inbox: %w", err)
	}
	queryer, err := inboxQueryer(db)
	if err != nil {
		return nil, fmt.Errorf("get inbox: %w", err)
	}
	items, err := queryInboxItems(ctx, queryer, chatID, "pending")
	if err != nil {
		return nil, fmt.Errorf("get inbox: %w", err)
	}
	return items, nil
}

// CancelInboxPrompt removes a pending prompt from delivery while retaining its
// submission receipt. Its snapshotted children are discarded.
func CancelInboxPrompt(ctx context.Context, db databaseExecutor, chatID, inboxID int64) error {
	err := executeAtomically(ctx, db, func(executor databaseExecutor) error {
		if err := reserveChat(ctx, executor, chatID); err != nil {
			return err
		}
		result, err := executor.ExecContext(ctx, `
			UPDATE inbox_items
			SET status = 'cancelled'
			WHERE id = ? AND chat_id = ? AND status = 'pending'
		`, inboxID, chatID)
		if err != nil {
			return fmt.Errorf("cancel inbox item: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("cancel inbox rows affected: %w", err)
		}
		if affected == 0 {
			return ErrInboxConflict
		}
		return discardInboxPayload(ctx, executor, inboxID)
	})
	if err != nil {
		return fmt.Errorf("cancel inbox prompt: %w", err)
	}
	return nil
}

// SetInboxDelivery changes how a pending prompt will be promoted: "steer"
// interrupts the active turn, while "queue" waits for the next user input.
func SetInboxDelivery(ctx context.Context, db databaseExecutor, chatID, inboxID int64, delivery string) error {
	delivery = strings.TrimSpace(delivery)
	if delivery != "steer" && delivery != "queue" {
		return fmt.Errorf("set inbox delivery: invalid delivery %q", delivery)
	}
	err := executeAtomically(ctx, db, func(executor databaseExecutor) error {
		if err := reserveChat(ctx, executor, chatID); err != nil {
			return err
		}
		result, err := executor.ExecContext(ctx, `
			UPDATE inbox_items
			SET delivery = ?
			WHERE id = ? AND chat_id = ? AND status = 'pending'
		`, delivery, inboxID, chatID)
		if err != nil {
			return fmt.Errorf("set inbox delivery: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("set inbox delivery rows affected: %w", err)
		}
		if affected == 0 {
			return ErrInboxConflict
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("set inbox delivery: %w", err)
	}
	return nil
}

// CheckpointCompletion atomically appends generated messages and promotes
// pending prompts. promotable is "", "steer", or "input". Stored history must
// still match expected or the whole checkpoint rolls back.
func CheckpointCompletion(ctx context.Context, db *sql.DB, chatID int64, expected MessageSnapshot, generated []llm.Message, promotable string) (CheckpointResult, error) {
	return checkpointCompletion(ctx, db, chatID, expected, generated, promotable, nil)
}

// CheckpointCompletionWithState is CheckpointCompletion with an explicit
// completion-state snapshot. Queued prompt delivery always records that
// prompt's captured selection; otherwise the supplied state is written in the
// same transaction. Any delivery forces Pending true.
func CheckpointCompletionWithState(ctx context.Context, db *sql.DB, chatID int64, expected MessageSnapshot, generated []llm.Message, promotable string, state CompletionState) (CheckpointResult, error) {
	normalized, err := normalizeCompletionState(state)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint completion: %w", err)
	}
	return checkpointCompletion(ctx, db, chatID, expected, generated, promotable, &normalized)
}

func checkpointCompletion(ctx context.Context, db *sql.DB, chatID int64, expected MessageSnapshot, generated []llm.Message, promotable string, override *CompletionState) (CheckpointResult, error) {
	if expected.chatID != chatID {
		return CheckpointResult{}, ErrConversationConflict
	}
	switch promotable {
	case "", "steer", "input":
	default:
		return CheckpointResult{}, fmt.Errorf("checkpoint completion: invalid promotion mode %q", promotable)
	}
	userID, err := requireUserID(ctx)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint completion: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("begin completion checkpoint: %w", err)
	}
	defer tx.Rollback()

	// First write reserves the SQLite writer lock before history validation.
	result, err := tx.ExecContext(ctx, `UPDATE chats SET id = id WHERE id = ? AND user_id = ?`, chatID, userID)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("reserve chat for checkpoint: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("reserve checkpoint rows affected: %w", err)
	}
	if affected == 0 {
		return CheckpointResult{}, ErrChatNotFound
	}

	current, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("reload messages for checkpoint: %w", err)
	}
	if current.version != expected.version {
		return CheckpointResult{}, ErrConversationConflict
	}

	pending, err := queryInboxItems(ctx, tx, chatID, "pending")
	if err != nil {
		return CheckpointResult{}, err
	}
	deliveries := selectInboxDeliveries(pending, promotable)
	changed := len(generated) > 0 || len(deliveries) > 0
	if changed {
		if err := DiscardUndoneMessages(ctx, tx, chatID); err != nil {
			return CheckpointResult{}, err
		}
	}

	nextPosition := current.version.lastPosition + 1
	appended := make([]llm.Message, 0, len(generated))
	for _, message := range generated {
		id, err := insertMessage(ctx, tx, chatID, nextPosition, message)
		if err != nil {
			return CheckpointResult{}, fmt.Errorf("append checkpoint message: %w", err)
		}
		message.ID = id
		appended = append(appended, message)
		nextPosition++
	}

	var queueDelivery *InboxItem
	delivered := make([]InboxItem, 0, len(deliveries))
	for _, item := range deliveries {
		var err error
		if item.Delivery == "queue" {
			err = replaceChatOptions(ctx, tx, chatID, item.Tools, item.AppendIDs)
		} else {
			// A steering turn may update append selections for the next user
			// turn, but never the tools available to the active completion.
			err = replaceChatPromptAppends(ctx, tx, chatID, item.AppendIDs)
		}
		if err != nil {
			return CheckpointResult{}, err
		}

		id, err := insertMessage(ctx, tx, chatID, nextPosition, item.Message)
		if err != nil {
			return CheckpointResult{}, fmt.Errorf("deliver inbox prompt: %w", err)
		}
		nextPosition++

		marked, err := tx.ExecContext(ctx, `
			UPDATE inbox_items
			SET status = 'delivered', message_id = ?
			WHERE id = ? AND chat_id = ? AND status = 'pending'
		`, id, item.ID, chatID)
		if err != nil {
			return CheckpointResult{}, fmt.Errorf("mark inbox prompt delivered: %w", err)
		}
		affected, err := marked.RowsAffected()
		if err != nil {
			return CheckpointResult{}, fmt.Errorf("mark inbox prompt rows affected: %w", err)
		}
		if affected == 0 {
			return CheckpointResult{}, ErrInboxConflict
		}
		if err := discardInboxPayload(ctx, tx, item.ID); err != nil {
			return CheckpointResult{}, err
		}
		item.Message.ID = id
		item.Status = "delivered"
		if item.Delivery == "queue" {
			captured := item
			queueDelivery = &captured
		}
		delivered = append(delivered, item)
	}

	if changed || override != nil {
		switch {
		case queueDelivery != nil:
			// The queued prompt owns the next busy period. Persist its captured
			// selection atomically so a restart cannot resume it with the
			// previous turn's model or tools.
			queuedState := CompletionState{Pending: true, Model: queueDelivery.Model, Tools: queueDelivery.Tools}
			if err := writeCompletionState(ctx, tx, chatID, queuedState); err != nil {
				return CheckpointResult{}, err
			}
		case override != nil:
			state := *override
			state.Pending = checkpointPendingState(delivered, generated, override.Pending)
			if err := writeCompletionState(ctx, tx, chatID, state); err != nil {
				return CheckpointResult{}, err
			}
		case len(delivered) > 0:
			// Steering keeps the active turn's stored model and tool selection.
			if err := recordCheckpointCompletionState(ctx, tx, chatID, true); err != nil {
				return CheckpointResult{}, err
			}
		default:
			if err := recordCheckpointCompletionState(ctx, tx, chatID, checkpointPendingState(delivered, generated, false)); err != nil {
				return CheckpointResult{}, err
			}
		}
	}

	snapshot, err := getMessageSnapshot(ctx, tx, chatID)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("reload checkpoint messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CheckpointResult{}, fmt.Errorf("commit checkpoint: %w", err)
	}
	return CheckpointResult{Snapshot: snapshot, Appended: appended, Delivered: delivered}, nil
}

func checkpointPendingState(delivered []InboxItem, generated []llm.Message, fallback bool) bool {
	if len(delivered) > 0 {
		return true
	}
	if len(generated) == 0 {
		return fallback
	}
	last := generated[len(generated)-1]
	return !(last.Role == "assistant" && len(last.ToolCalls) == 0)
}

// recordCheckpointCompletionState updates only the pending flag so a crash
// after persisting a final assistant turn does not replay it. Model and tool
// snapshots belong to the caller's completed work and are preserved.
func recordCheckpointCompletionState(ctx context.Context, db databaseExecutor, chatID int64, pending bool) error {
	value := 0
	if pending {
		value = 1
	}
	result, err := db.ExecContext(ctx, `
		UPDATE completion_state
		SET pending = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE chat_id = ?
	`, value, chatID)
	if err != nil {
		return fmt.Errorf("update completion state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update completion state rows affected: %w", err)
	}
	if affected > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO completion_state (chat_id, pending, model)
		VALUES (?, ?, '')
	`, chatID, value); err != nil {
		return fmt.Errorf("create completion state: %w", err)
	}
	return nil
}

// SetCompletionState stores the durable pending-completion state for a chat.
func SetCompletionState(ctx context.Context, db databaseExecutor, chatID int64, state CompletionState) error {
	normalized, err := normalizeCompletionState(state)
	if err != nil {
		return fmt.Errorf("set completion state: %w", err)
	}

	err = executeAtomically(ctx, db, func(executor databaseExecutor) error {
		if err := reserveChat(ctx, executor, chatID); err != nil {
			return err
		}
		return writeCompletionState(ctx, executor, chatID, normalized)
	})
	if err != nil {
		return fmt.Errorf("set completion state: %w", err)
	}
	return nil
}

func normalizeCompletionState(state CompletionState) (CompletionState, error) {
	state.Model = strings.TrimSpace(state.Model)
	if len([]byte(state.Model)) > maxInboxModelBytes {
		return CompletionState{}, fmt.Errorf("model exceeds %d bytes", maxInboxModelBytes)
	}
	tools, err := normalizeCompletionTools(state.Tools)
	if err != nil {
		return CompletionState{}, err
	}
	state.Tools = tools
	return state, nil
}

// writeCompletionState assumes the caller holds the chat writer lock.
func writeCompletionState(ctx context.Context, db databaseExecutor, chatID int64, state CompletionState) error {
	pending := 0
	if state.Pending {
		pending = 1
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO completion_state (chat_id, pending, model)
		VALUES (?, ?, ?)
		ON CONFLICT (chat_id) DO UPDATE SET
			pending = excluded.pending,
			model = excluded.model,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
	`, chatID, pending, state.Model); err != nil {
		return fmt.Errorf("store completion state: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_state_tools WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("clear completion state tools: %w", err)
	}
	for position, name := range state.Tools {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO completion_state_tools (chat_id, position, name)
			VALUES (?, ?, ?)
		`, chatID, position, name); err != nil {
			return fmt.Errorf("store completion state tool %d: %w", position, err)
		}
	}
	return nil
}

// GetCompletionState returns the durable pending-completion state for a chat.
// A chat without a stored state reports no pending completion.
func GetCompletionState(ctx context.Context, db databaseExecutor, chatID int64) (CompletionState, error) {
	if err := requireChatOwned(ctx, db, chatID); err != nil {
		return CompletionState{}, fmt.Errorf("get completion state: %w", err)
	}
	queryer, err := inboxQueryer(db)
	if err != nil {
		return CompletionState{}, fmt.Errorf("get completion state: %w", err)
	}

	var (
		pending int
		state   CompletionState
	)
	err = queryer.QueryRowContext(ctx, `
		SELECT pending, model
		FROM completion_state
		JOIN chats ON chats.id = completion_state.chat_id
		WHERE completion_state.chat_id = ? AND chats.user_id = ?
	`, chatID, UserID(ctx)).Scan(&pending, &state.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return CompletionState{}, nil
	}
	if err != nil {
		return CompletionState{}, fmt.Errorf("get completion state: %w", err)
	}
	state.Pending = pending == 1
	state.Tools = []string{}

	rows, err := queryer.QueryContext(ctx, `
		SELECT tools.name
		FROM completion_state_tools AS tools
		JOIN completion_state ON completion_state.chat_id = tools.chat_id
		JOIN chats ON chats.id = completion_state.chat_id
		WHERE completion_state.chat_id = ? AND chats.user_id = ?
		ORDER BY tools.position
	`, chatID, UserID(ctx))
	if err != nil {
		return CompletionState{}, fmt.Errorf("get completion state tools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return CompletionState{}, fmt.Errorf("scan completion state tool: %w", err)
		}
		state.Tools = append(state.Tools, name)
	}
	if err := rows.Err(); err != nil {
		return CompletionState{}, fmt.Errorf("iterate completion state tools: %w", err)
	}
	return state, nil
}

func prepareInboxItem(item InboxItem) (InboxItem, error) {
	item.SubmissionID = strings.TrimSpace(item.SubmissionID)
	if item.SubmissionID == "" {
		return InboxItem{}, errors.New("submission ID is required")
	}
	if len([]byte(item.SubmissionID)) > maxInboxIDBytes {
		return InboxItem{}, fmt.Errorf("submission ID exceeds %d bytes", maxInboxIDBytes)
	}
	item.Delivery = strings.TrimSpace(item.Delivery)
	if item.Delivery != "steer" && item.Delivery != "queue" {
		return InboxItem{}, fmt.Errorf("invalid delivery %q", item.Delivery)
	}
	item.Model = strings.TrimSpace(item.Model)
	if len([]byte(item.Model)) > maxInboxModelBytes {
		return InboxItem{}, fmt.Errorf("model exceeds %d bytes", maxInboxModelBytes)
	}
	switch item.Status {
	case "", "pending":
		item.Status = "pending"
	default:
		return InboxItem{}, fmt.Errorf("invalid status %q", item.Status)
	}

	message := item.Message
	if message.Role != "user" {
		return InboxItem{}, errors.New("message role must be user")
	}
	if message.ID != 0 || message.Model != "" || message.TotalTokens != nil || message.Cost != nil ||
		len(message.ToolCalls) > 0 || message.ToolCallID != "" || !message.ProviderMetadata.IsZero() {
		return InboxItem{}, errors.New("user message contains non-user metadata")
	}
	for position, attachment := range message.Attachments {
		if attachment.MediaType == "" || len(attachment.Data) == 0 || attachment.Width < 0 || attachment.Height < 0 {
			return InboxItem{}, fmt.Errorf("attachment %d has invalid media type, data, or dimensions", position)
		}
	}
	message.ID = 0
	item.Message = message

	tools, err := normalizeInboxTools(item.Tools)
	if err != nil {
		return InboxItem{}, err
	}
	item.Tools = tools

	appendIDs := make([]string, 0, len(item.AppendIDs))
	seen := make(map[string]struct{}, len(item.AppendIDs))
	for _, id := range item.AppendIDs {
		id = strings.TrimSpace(id)
		if err := ValidatePromptAppendID(id); err != nil {
			return InboxItem{}, err
		}
		if _, ok := seen[id]; ok {
			return InboxItem{}, fmt.Errorf("prompt append %q is duplicated", id)
		}
		seen[id] = struct{}{}
		appendIDs = append(appendIDs, id)
	}
	item.AppendIDs = appendIDs
	return item, nil
}

func normalizeInboxTools(names []string) ([]string, error) {
	tools := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("tool name is required")
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("tool %q is duplicated", name)
		}
		seen[name] = struct{}{}
		tools = append(tools, name)
	}
	return tools, nil
}

func normalizeCompletionTools(names []string) ([]string, error) {
	tools := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		tools = append(tools, name)
	}
	return tools, nil
}

func selectInboxDeliveries(pending []InboxItem, promotable string) []InboxItem {
	if promotable == "" {
		return nil
	}
	steers := make([]InboxItem, 0, len(pending))
	firstQueue := -1
	for index, item := range pending {
		if item.Delivery == "steer" {
			steers = append(steers, item)
			continue
		}
		if firstQueue < 0 {
			firstQueue = index
		}
	}
	if promotable == "steer" || len(steers) > 0 {
		return steers
	}
	if firstQueue >= 0 {
		return []InboxItem{pending[firstQueue]}
	}
	return nil
}

func inboxItemBySubmission(ctx context.Context, db messageQueryer, chatID int64, submissionID string) (InboxItem, bool, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return InboxItem{}, false, err
	}
	item := InboxItem{}
	var (
		messageID     sql.NullInt64
		sealedContent string
	)
	err = db.QueryRowContext(ctx, `
		SELECT items.id, items.submission_id, items.delivery, items.model, items.status, items.content, items.message_id
		FROM inbox_items AS items
		JOIN chats ON chats.id = items.chat_id
		WHERE chats.user_id = ? AND items.chat_id = ? AND items.submission_id = ?
	`, userID, chatID, submissionID).Scan(
		&item.ID, &item.SubmissionID, &item.Delivery, &item.Model, &item.Status, &sealedContent, &messageID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxItem{}, false, nil
	}
	if err != nil {
		return InboxItem{}, false, fmt.Errorf("get inbox submission receipt: %w", err)
	}
	content, err := DecryptStorageText(ctx, inboxContentScope, inboxStorageRowID(item.ID), sealedContent)
	if err != nil {
		return InboxItem{}, false, fmt.Errorf("decrypt inbox submission receipt content: %w", err)
	}
	item.Message.Content = content
	if messageID.Valid {
		item.Message.ID = messageID.Int64
	}
	item.Message.Role = "user"
	item.Tools = []string{}
	item.AppendIDs = []string{}
	items := []InboxItem{item}
	if err := loadInboxChildren(ctx, db, userID, chatID, items); err != nil {
		return InboxItem{}, false, err
	}
	return items[0], true, nil
}

func queryInboxItems(ctx context.Context, db messageQueryer, chatID int64, status string) ([]InboxItem, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT items.id, items.submission_id, items.delivery, items.model, items.status, items.content, items.message_id
		FROM inbox_items AS items
		JOIN chats ON chats.id = items.chat_id
		WHERE chats.user_id = ?
			AND items.chat_id = ?
			AND items.status = ?
		ORDER BY items.id
	`, userID, chatID, status)
	if err != nil {
		return nil, fmt.Errorf("get inbox items: %w", err)
	}
	defer rows.Close()

	items := []InboxItem{}
	for rows.Next() {
		var (
			item          InboxItem
			messageID     sql.NullInt64
			sealedContent string
		)
		if err := rows.Scan(&item.ID, &item.SubmissionID, &item.Delivery, &item.Model, &item.Status, &sealedContent, &messageID); err != nil {
			return nil, fmt.Errorf("scan inbox item: %w", err)
		}
		content, err := DecryptStorageText(ctx, inboxContentScope, inboxStorageRowID(item.ID), sealedContent)
		if err != nil {
			return nil, fmt.Errorf("decrypt inbox item %d content: %w", item.ID, err)
		}
		item.Message.Content = content
		if messageID.Valid {
			item.Message.ID = messageID.Int64
		}
		item.Message.Role = "user"
		item.Tools = []string{}
		item.AppendIDs = []string{}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate inbox items: %w", err)
	}
	if err := loadInboxChildren(ctx, db, userID, chatID, items); err != nil {
		return nil, err
	}
	return items, nil
}

func loadInboxChildren(ctx context.Context, db messageQueryer, userID, chatID int64, items []InboxItem) error {
	if len(items) == 0 {
		return nil
	}
	indexes := make(map[int64]int, len(items))
	arguments := make([]any, 0, len(items)+2)
	arguments = append(arguments, userID, chatID)
	for index := range items {
		indexes[items[index].ID] = index
		arguments = append(arguments, items[index].ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	condition := `JOIN inbox_items AS items ON items.id = child.inbox_id
		JOIN chats ON chats.id = items.chat_id
		WHERE chats.user_id = ? AND items.chat_id = ? AND child.inbox_id IN (` + placeholders + `)`

	rows, err := db.QueryContext(ctx, `
		SELECT child.inbox_id, child.name
		FROM inbox_tools AS child
		`+condition+`
		ORDER BY child.inbox_id, child.position
	`, arguments...)
	if err != nil {
		return fmt.Errorf("get inbox tools: %w", err)
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return fmt.Errorf("scan inbox tool: %w", err)
		}
		items[indexes[id]].Tools = append(items[indexes[id]].Tools, name)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close inbox tools: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate inbox tools: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT child.inbox_id, child.prompt_append_id
		FROM inbox_prompt_appends AS child
		`+condition+`
		ORDER BY child.inbox_id, child.position
	`, arguments...)
	if err != nil {
		return fmt.Errorf("get inbox prompt appends: %w", err)
	}
	for rows.Next() {
		var id int64
		var appendID string
		if err := rows.Scan(&id, &appendID); err != nil {
			rows.Close()
			return fmt.Errorf("scan inbox prompt append: %w", err)
		}
		items[indexes[id]].AppendIDs = append(items[indexes[id]].AppendIDs, appendID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close inbox prompt appends: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate inbox prompt appends: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT child.inbox_id, child.position, child.text
		FROM inbox_prompt_append_texts AS child
		`+condition+`
		ORDER BY child.inbox_id, child.position
	`, arguments...)
	if err != nil {
		return fmt.Errorf("get inbox prompt append texts: %w", err)
	}
	for rows.Next() {
		var (
			id         int64
			position   int
			sealedText string
		)
		if err := rows.Scan(&id, &position, &sealedText); err != nil {
			rows.Close()
			return fmt.Errorf("scan inbox prompt append text: %w", err)
		}
		text, err := DecryptStorageText(ctx, inboxAppendTextScope, inboxChildStorageRowID(id, position), sealedText)
		if err != nil {
			rows.Close()
			return fmt.Errorf("decrypt inbox prompt append text: %w", err)
		}
		items[indexes[id]].Message.PromptAppendTexts = append(items[indexes[id]].Message.PromptAppendTexts, text)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close inbox prompt append texts: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate inbox prompt append texts: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT child.inbox_id, child.position, child.filename, child.media_type, child.width, child.height, child.data
		FROM inbox_attachments AS child
		`+condition+`
		ORDER BY child.inbox_id, child.position
	`, arguments...)
	if err != nil {
		return fmt.Errorf("get inbox attachments: %w", err)
	}
	for rows.Next() {
		var (
			id             int64
			position       int
			sealedFilename string
			mediaType      string
			width          int
			height         int
			sealedData     []byte
		)
		if err := rows.Scan(&id, &position, &sealedFilename, &mediaType, &width, &height, &sealedData); err != nil {
			rows.Close()
			return fmt.Errorf("scan inbox attachment: %w", err)
		}
		rowID := inboxChildStorageRowID(id, position)
		filename, err := DecryptStorageText(ctx, inboxAttachmentFilenameScope, rowID, sealedFilename)
		if err != nil {
			rows.Close()
			return fmt.Errorf("decrypt inbox attachment filename: %w", err)
		}
		data, err := DecryptStorageBytes(ctx, inboxAttachmentDataScope, rowID, sealedData)
		if err != nil {
			rows.Close()
			return fmt.Errorf("decrypt inbox attachment data: %w", err)
		}
		attachment := llm.UserAttachment{
			Filename:  filename,
			MediaType: mediaType,
			Width:     width,
			Height:    height,
			Data:      append([]byte(nil), data...),
		}
		items[indexes[id]].Message.Attachments = append(items[indexes[id]].Message.Attachments, attachment)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close inbox attachments: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate inbox attachments: %w", err)
	}
	return nil
}

func insertInboxChildren(ctx context.Context, db databaseExecutor, inboxID int64, item InboxItem) error {
	for position, name := range item.Tools {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inbox_tools (inbox_id, position, name)
			VALUES (?, ?, ?)
		`, inboxID, position, name); err != nil {
			return fmt.Errorf("store inbox tool %d: %w", position, err)
		}
	}
	for position, id := range item.AppendIDs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inbox_prompt_appends (inbox_id, position, prompt_append_id)
			VALUES (?, ?, ?)
		`, inboxID, position, id); err != nil {
			return fmt.Errorf("store inbox prompt append %d: %w", position, err)
		}
	}
	for position, text := range item.Message.PromptAppendTexts {
		sealedText, err := EncryptStorageText(ctx, inboxAppendTextScope, inboxChildStorageRowID(inboxID, position), text)
		if err != nil {
			return fmt.Errorf("encrypt inbox prompt append text %d: %w", position, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inbox_prompt_append_texts (inbox_id, position, text)
			VALUES (?, ?, ?)
		`, inboxID, position, sealedText); err != nil {
			return fmt.Errorf("store inbox prompt append text %d: %w", position, err)
		}
	}
	for position, attachment := range item.Message.Attachments {
		rowID := inboxChildStorageRowID(inboxID, position)
		sealedFilename, err := EncryptStorageText(ctx, inboxAttachmentFilenameScope, rowID, attachment.Filename)
		if err != nil {
			return fmt.Errorf("encrypt inbox attachment %d filename: %w", position, err)
		}
		sealedData, err := EncryptStorageBytes(ctx, inboxAttachmentDataScope, rowID, attachment.Data)
		if err != nil {
			return fmt.Errorf("encrypt inbox attachment %d data: %w", position, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inbox_attachments
				(inbox_id, position, filename, media_type, width, height, data)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, inboxID, position, sealedFilename, attachment.MediaType, attachment.Width, attachment.Height, sealedData); err != nil {
			return fmt.Errorf("store inbox attachment %d: %w", position, err)
		}
	}
	return nil
}

func discardInboxPayload(ctx context.Context, db databaseExecutor, inboxID int64) error {
	for _, table := range []string{
		"inbox_tools",
		"inbox_prompt_appends",
		"inbox_prompt_append_texts",
		"inbox_attachments",
	} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE inbox_id = ?`, inboxID); err != nil {
			return fmt.Errorf("discard %s for inbox item %d: %w", table, inboxID, err)
		}
	}
	return nil
}

func replaceChatPromptAppends(ctx context.Context, db databaseExecutor, chatID int64, appendIDs []string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM chat_prompt_appends WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("clear chat prompt appends: %w", err)
	}
	for position, id := range appendIDs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO chat_prompt_appends (chat_id, position, prompt_append_id)
			VALUES (?, ?, ?)
		`, chatID, position, id); err != nil {
			return fmt.Errorf("store prompt append %d for chat %d: %w", position, chatID, err)
		}
	}
	return nil
}

func inboxQueryer(db databaseExecutor) (messageQueryer, error) {
	queryer, ok := db.(messageQueryer)
	if !ok {
		return nil, fmt.Errorf("unsupported database executor %T", db)
	}
	return queryer, nil
}
