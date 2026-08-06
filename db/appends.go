package kritui_db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	promptAppendsSetting     = "prompt_appends"
	maxPromptAppends         = 32
	maxPromptAppendNameRunes = 80
	maxPromptAppendTextBytes = 16 << 10
	defaultLinkCheckAppendID = "link-check"
	defaultResearchAppendID  = "research"
)

// PromptAppend is a named text fragment that can be added to a user message.
type PromptAppend struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
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

// GetPromptAppends returns stored presets, or built-in presets when none have
// been configured. Stored values that fail to decode or validate return an
// error instead of falling back, so corruption is never silently replaced by
// defaults.
func GetPromptAppends(ctx context.Context, db *sql.DB) ([]PromptAppend, error) {
	var encoded string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, promptAppendsSetting).Scan(&encoded)
	if err == sql.ErrNoRows {
		return DefaultPromptAppends(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("get prompt appends: %w", err)
	}

	var values []PromptAppend
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return nil, fmt.Errorf("decode prompt appends: %w", err)
	}
	normalized, err := normalizePromptAppends(values)
	if err != nil {
		return nil, fmt.Errorf("normalize prompt appends: %w", err)
	}
	return normalized, nil
}

// SetPromptAppends replaces all configurable prompt presets. Replacement
// happens atomically with pruning removed preset IDs from every chat's
// selected appends, so stale references never survive a settings change.
func SetPromptAppends(ctx context.Context, db *sql.DB, values []PromptAppend) error {
	normalized, err := normalizePromptAppends(values)
	if err != nil {
		return fmt.Errorf("set prompt appends: %w", err)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("encode prompt appends: %w", err)
	}
	encodedIDs, err := encodePromptAppendIDs(PromptAppendIDs(normalized))
	if err != nil {
		return fmt.Errorf("encode prompt append IDs: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set prompt appends: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value
	`, promptAppendsSetting, string(encoded)); err != nil {
		return fmt.Errorf("set prompt appends: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE chats
		SET appends = (
			SELECT COALESCE(json_group_array(value), '[]')
			FROM (
				SELECT value
				FROM json_each(chats.appends)
				WHERE value IN (SELECT value FROM json_each(?))
				ORDER BY CAST(key AS INTEGER)
			)
		)
		WHERE json_valid(appends)
	`, encodedIDs); err != nil {
		return fmt.Errorf("prune chat prompt appends: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set prompt appends: %w", err)
	}
	return nil
}

// PromptAppendIDs returns append IDs in their input order.
func PromptAppendIDs(values []PromptAppend) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
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

func encodePromptAppendIDs(ids []string) (string, error) {
	if ids == nil {
		ids = []string{}
	}
	encoded, err := json.Marshal(ids)
	return string(encoded), err
}

func decodePromptAppendIDs(encoded string) ([]string, error) {
	var ids []string
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
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
		if value.ID == "" {
			return nil, fmt.Errorf("prompt append ID is required")
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
