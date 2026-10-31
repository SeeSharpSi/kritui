package kritui_db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
)

const (
	maxPromptAppends         = 32
	maxPromptAppendIDBytes   = 64
	maxPromptAppendNameRunes = 80
	maxPromptAppendTextBytes = 16 << 10
	defaultLinkCheckAppendID = "link-check"
	defaultResearchAppendID  = "research"
	// PDFImagesAppendID is the built-in append used when a model cannot read a
	// PDF directly and its pages are sent as images instead.
	PDFImagesAppendID = "pdf-images"
)

// PromptAppend is a named text fragment that can be added to a user message.
type PromptAppend struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Text             string `json:"text"`
	EnabledByDefault bool   `json:"enabled_by_default,omitempty"`
}

//go:embed default_appends/*.md
var defaultAppendFiles embed.FS

// DefaultPromptAppends returns built-in presets. Their text lives in editable
// embedded Markdown files so deployments can change defaults at build time.
func DefaultPromptAppends() []PromptAppend {
	return []PromptAppend{
		{
			ID:   defaultLinkCheckAppendID,
			Name: "link check",
			Text: embeddedAppendText("default_appends/link-check.md"),
		},
		{
			ID:   defaultResearchAppendID,
			Name: "research",
			Text: embeddedAppendText("default_appends/research.md"),
		},
	}
}

func embeddedAppendText(name string) string {
	content, err := defaultAppendFiles.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("read embedded prompt append %q: %v", name, err))
	}
	return strings.TrimSpace(string(content))
}

// PDFImagesAppendText returns the hidden append snapshotted when a PDF is
// converted to page images. It is not part of DefaultPromptAppends and does not
// appear in the settings or chat append pickers.
func PDFImagesAppendText() string {
	return embeddedAppendText("default_appends/pdf-images.md")
}

// rowQueryer is satisfied by both *sql.DB and *sql.Tx.
type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// GetPromptAppends returns stored presets, or built-in presets when none have
// been configured.
func GetPromptAppends(ctx context.Context, db rowQueryer) ([]PromptAppend, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get prompt append state: %w", err)
	}
	var configured bool
	if err := db.QueryRowContext(ctx, `SELECT prompt_appends_configured FROM settings WHERE id = ?`, userID).Scan(&configured); err != nil {
		return nil, fmt.Errorf("get prompt append state: %w", err)
	}
	if !configured {
		return DefaultPromptAppends(), nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, name, text, enabled_by_default
		FROM prompt_appends
		WHERE user_id = ?
		ORDER BY position
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("get prompt appends: %w", err)
	}
	defer rows.Close()
	values := []PromptAppend{}
	for rows.Next() {
		var value PromptAppend
		if err := rows.Scan(&value.ID, &value.Name, &value.Text, &value.EnabledByDefault); err != nil {
			return nil, fmt.Errorf("scan prompt append: %w", err)
		}
		decryptedName, err := DecryptStorageText(ctx, "prompt_appends.name", value.ID, value.Name)
		if err != nil {
			return nil, fmt.Errorf("get prompt appends: %w", err)
		}
		decryptedText, err := DecryptStorageText(ctx, "prompt_appends.text", value.ID, value.Text)
		if err != nil {
			return nil, fmt.Errorf("get prompt appends: %w", err)
		}
		value.Name = decryptedName
		value.Text = decryptedText
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompt appends: %w", err)
	}
	normalized, err := normalizePromptAppends(values)
	if err != nil {
		return nil, fmt.Errorf("normalize prompt appends: %w", err)
	}
	return normalized, nil
}

// setPromptAppends writes definitions and prunes removed chat selections.
func setPromptAppends(ctx context.Context, db settingWriter, values []PromptAppend) error {
	userID, err := requireUserID(ctx)
	if err != nil {
		return fmt.Errorf("set prompt appends: %w", err)
	}
	normalized, err := normalizePromptAppends(values)
	if err != nil {
		return fmt.Errorf("set prompt appends: %w", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM prompt_appends WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clear prompt appends: %w", err)
	}
	for position, value := range normalized {
		encryptedName, err := EncryptStorageText(ctx, "prompt_appends.name", value.ID, value.Name)
		if err != nil {
			return fmt.Errorf("set prompt appends: %w", err)
		}
		encryptedText, err := EncryptStorageText(ctx, "prompt_appends.text", value.ID, value.Text)
		if err != nil {
			return fmt.Errorf("set prompt appends: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO prompt_appends (user_id, id, position, name, text, enabled_by_default)
			VALUES (?, ?, ?, ?, ?, ?)
		`, userID, value.ID, position, encryptedName, encryptedText, value.EnabledByDefault); err != nil {
			return fmt.Errorf("store prompt append %q: %w", value.ID, err)
		}
	}

	if _, err := db.ExecContext(ctx, `
		DELETE FROM chat_prompt_appends
		WHERE chat_id IN (SELECT id FROM chats WHERE user_id = ?)
			AND prompt_append_id NOT IN (
				SELECT id FROM prompt_appends WHERE user_id = ?
			)
	`, userID, userID); err != nil {
		return fmt.Errorf("prune chat prompt appends: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET prompt_appends_configured = 1 WHERE id = ?`, userID); err != nil {
		return fmt.Errorf("mark prompt appends configured: %w", err)
	}
	return nil
}

// ValidatePromptAppendID checks a prompt append ID against the single ID
// contract shared by every path that persists or re-renders definitions. A
// valid ID holds at most 64 ASCII bytes; its first and last characters are
// lowercase ASCII letters or digits; interior characters are lowercase ASCII
// letters, digits, or hyphens. Single-character alphanumeric IDs are valid.
// Generated IDs ("append-" plus 16 lowercase hex digits) and built-in IDs
// ("link-check", "research") satisfy the contract.
func ValidatePromptAppendID(id string) error {
	if id == "" {
		return fmt.Errorf("prompt append ID is required")
	}
	if len(id) > maxPromptAppendIDBytes {
		return fmt.Errorf("prompt append %q exceeds %d bytes", id, maxPromptAppendIDBytes)
	}
	for index := 0; index < len(id); index++ {
		valid := isASCIILetterOrDigit(id[index])
		if !valid && index > 0 && index < len(id)-1 && id[index] == '-' {
			valid = true
		}
		if !valid {
			return fmt.Errorf("prompt append %q has an invalid character at byte %d", id, index)
		}
	}
	return nil
}

func isASCIILetterOrDigit(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// PromptAppendIDs returns append IDs in their input order.
func PromptAppendIDs(values []PromptAppend) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids
}

// DefaultPromptAppendIDs returns IDs enabled by default in input order.
func DefaultPromptAppendIDs(values []PromptAppend) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		if value.EnabledByDefault {
			ids = append(ids, value.ID)
		}
	}
	return ids
}

// ValidatePromptAppends checks preset IDs, names, text, and collection size.
func ValidatePromptAppends(values []PromptAppend) error {
	_, err := normalizePromptAppends(values)
	return err
}

// SelectPromptAppends resolves submitted preset IDs and rejects unknown or
// repeated IDs before prompt text is applied.
func SelectPromptAppends(values []PromptAppend, ids []string) ([]PromptAppend, error) {
	byID := make(map[string]PromptAppend, len(values))
	for _, value := range values {
		byID[value.ID] = value
	}

	selected := make([]PromptAppend, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		value, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown prompt append %q", id)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("prompt append %q selected more than once", id)
		}
		seen[id] = struct{}{}
		selected = append(selected, value)
	}
	return selected, nil
}

func normalizePromptAppends(values []PromptAppend) ([]PromptAppend, error) {
	if len(values) > maxPromptAppends {
		return nil, fmt.Errorf("at most %d prompt appends are allowed", maxPromptAppends)
	}

	normalized := make([]PromptAppend, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		value.Name = strings.TrimSpace(value.Name)
		value.Text = strings.TrimSpace(value.Text)
		if err := ValidatePromptAppendID(value.ID); err != nil {
			return nil, err
		}
		if value.Name == "" {
			return nil, fmt.Errorf("prompt append %q name is required", value.ID)
		}
		if len([]rune(value.Name)) > maxPromptAppendNameRunes {
			return nil, fmt.Errorf("prompt append %q name exceeds %d characters", value.ID, maxPromptAppendNameRunes)
		}
		if value.Text == "" {
			return nil, fmt.Errorf("prompt append %q text is required", value.ID)
		}
		if len([]byte(value.Text)) > maxPromptAppendTextBytes {
			return nil, fmt.Errorf("prompt append %q text exceeds %d bytes", value.ID, maxPromptAppendTextBytes)
		}
		if _, ok := seen[value.ID]; ok {
			return nil, fmt.Errorf("prompt append %q is duplicated", value.ID)
		}
		seen[value.ID] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}
