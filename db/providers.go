package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// EnvProviderID identifies the synthetic environment provider. It is never
// stored in the providers table; callers resolve it from process configuration.
const EnvProviderID int64 = -1

const (
	providerEndpointScope = "provider.endpoint"
	providerAPIKeyScope   = "provider.api_key"
	maxProviderNameBytes  = 100
)

// ProviderSettings contains provider values safe to render in settings pages.
// APIKeyConfigured reports secret presence without exposing the secret.
type ProviderSettings struct {
	ID               int64
	Name             string
	Endpoint         string
	APIKeyConfigured bool
	Position         int
}

// ProviderConfig contains credentials needed for model completion. Keep this
// type out of template and HTTP response data.
type ProviderConfig struct {
	ID       int64
	Name     string
	Endpoint string
	APIKey   string
}

// ProviderDraft describes one desired provider during an atomic save. ID 0
// inserts a new provider; a positive ID updates an owned provider.
type ProviderDraft struct {
	ID           int64
	Name         string
	Endpoint     string
	APIKeyChange LLMAPIKeyChange
	APIKeyValue  string
}

func providerStorageRowID(providerID int64) string {
	return strconv.FormatInt(providerID, 10)
}

// ValidateProviderDraft checks one provider draft without touching storage.
func ValidateProviderDraft(draft ProviderDraft) error {
	name := strings.TrimSpace(draft.Name)
	if name == "" {
		return fmt.Errorf("save providers: name is required")
	}
	if len([]byte(name)) > maxProviderNameBytes {
		return fmt.Errorf("save providers: name must not exceed %d bytes", maxProviderNameBytes)
	}
	endpoint := strings.TrimSpace(draft.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("save providers: endpoint is required")
	}
	if err := ValidateLLMSettings(LLMSettingsUpdate{
		Endpoint:     endpoint,
		APIKeyChange: draft.APIKeyChange,
		APIKeyValue:  draft.APIKeyValue,
	}); err != nil {
		return err
	}
	if len(strings.TrimSpace(draft.APIKeyValue)) > 16384 {
		return fmt.Errorf("save providers: API key must not exceed 16384 bytes")
	}
	return nil
}

// ListProviderSettings returns providers in display order with secrets hidden.
func ListProviderSettings(ctx context.Context, db *sql.DB) ([]ProviderSettings, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, position, endpoint,
			CASE WHEN api_key IS NOT NULL AND trim(api_key) <> '' THEN 1 ELSE 0 END
		FROM providers
		WHERE user_id = ?
		ORDER BY position, id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	providers := []ProviderSettings{}
	for rows.Next() {
		var provider ProviderSettings
		var endpoint string
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.Position, &endpoint, &provider.APIKeyConfigured); err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		decrypted, err := DecryptStorageText(ctx, providerEndpointScope, providerStorageRowID(provider.ID), endpoint)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %d endpoint: %w", provider.ID, err)
		}
		provider.Endpoint = decrypted
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate providers: %w", err)
	}
	return providers, nil
}

// ListProviderConfigs returns providers in display order with credentials.
func ListProviderConfigs(ctx context.Context, db *sql.DB) ([]ProviderConfig, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provider configs: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, endpoint, api_key
		FROM providers
		WHERE user_id = ?
		ORDER BY position, id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list provider configs: %w", err)
	}
	defer rows.Close()

	providers := []ProviderConfig{}
	for rows.Next() {
		var provider ProviderConfig
		var endpoint string
		var apiKey sql.NullString
		if err := rows.Scan(&provider.ID, &provider.Name, &endpoint, &apiKey); err != nil {
			return nil, fmt.Errorf("scan provider config: %w", err)
		}
		provider.Endpoint, err = DecryptStorageText(ctx, providerEndpointScope, providerStorageRowID(provider.ID), endpoint)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %d endpoint: %w", provider.ID, err)
		}
		if apiKey.Valid {
			provider.APIKey, err = DecryptStorageText(ctx, providerAPIKeyScope, providerStorageRowID(provider.ID), apiKey.String)
			if err != nil {
				return nil, fmt.Errorf("decrypt provider %d API key: %w", provider.ID, err)
			}
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider configs: %w", err)
	}
	return providers, nil
}

// GetProviderConfig returns one owned provider with credentials.
func GetProviderConfig(ctx context.Context, db *sql.DB, id int64) (ProviderConfig, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("get provider config: %w", err)
	}
	var provider ProviderConfig
	var endpoint string
	var apiKey sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT id, name, endpoint, api_key
		FROM providers
		WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&provider.ID, &provider.Name, &endpoint, &apiKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderConfig{}, fmt.Errorf("get provider config: provider %d not found", id)
	}
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("get provider config: %w", err)
	}
	provider.Endpoint, err = DecryptStorageText(ctx, providerEndpointScope, providerStorageRowID(provider.ID), endpoint)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("decrypt provider %d endpoint: %w", provider.ID, err)
	}
	if apiKey.Valid {
		provider.APIKey, err = DecryptStorageText(ctx, providerAPIKeyScope, providerStorageRowID(provider.ID), apiKey.String)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("decrypt provider %d API key: %w", provider.ID, err)
		}
	}
	return provider, nil
}

// SaveProviders atomically replaces an account's provider list. Providers
// omitted from drafts are deleted, positions follow draft order, and blank or
// kept API keys preserve the stored secret.
func SaveProviders(ctx context.Context, db *sql.DB, drafts []ProviderDraft) ([]int64, error) {
	userID, err := requireUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("save providers: %w", err)
	}
	if err := validateProviderDrafts(drafts); err != nil {
		return nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin save providers: %w", err)
	}
	defer tx.Rollback()

	owned, err := ownedProviderIDs(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	kept := make(map[int64]struct{}, len(drafts))
	for _, draft := range drafts {
		if draft.ID == 0 {
			continue
		}
		if _, ok := owned[draft.ID]; !ok {
			return nil, fmt.Errorf("save providers: provider %d is not owned by this account", draft.ID)
		}
		kept[draft.ID] = struct{}{}
	}

	for id := range owned {
		if _, ok := kept[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM model_endpoint_preferences
			WHERE user_id = ? AND provider_id = ?
		`, userID, id); err != nil {
			return nil, fmt.Errorf("delete provider %d model preferences: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ? AND user_id = ?`, id, userID); err != nil {
			return nil, fmt.Errorf("delete provider %d: %w", id, err)
		}
	}

	// Move kept rows out of the way so final positions cannot collide during
	// reordering. The offset stays within SQLite's signed integer range.
	if _, err := tx.ExecContext(ctx, `
		UPDATE providers SET position = position + 1000000 WHERE user_id = ?
	`, userID); err != nil {
		return nil, fmt.Errorf("offset provider positions: %w", err)
	}

	ids := make([]int64, len(drafts))
	for position, draft := range drafts {
		name := strings.TrimSpace(draft.Name)
		endpoint := strings.TrimSpace(draft.Endpoint)
		if draft.ID == 0 {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO providers (user_id, position, name, endpoint, api_key)
				VALUES (?, ?, ?, 'pending', NULL)
			`, userID, position, name)
			if err != nil {
				return nil, fmt.Errorf("insert provider %q: %w", name, err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				return nil, fmt.Errorf("get inserted provider ID: %w", err)
			}
			if err := writeProviderFields(ctx, tx, userID, id, endpoint, draft); err != nil {
				return nil, err
			}
			ids[position] = id
			continue
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE providers SET position = ?, name = ? WHERE id = ? AND user_id = ?
		`, position, name, draft.ID, userID); err != nil {
			return nil, fmt.Errorf("update provider %d: %w", draft.ID, err)
		}
		if err := writeProviderFields(ctx, tx, userID, draft.ID, endpoint, draft); err != nil {
			return nil, err
		}
		ids[position] = draft.ID
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit save providers: %w", err)
	}
	return ids, nil
}

func validateProviderDrafts(drafts []ProviderDraft) error {
	names := make(map[string]struct{}, len(drafts))
	ids := make(map[int64]struct{}, len(drafts))
	for index, draft := range drafts {
		if draft.ID < 0 {
			return fmt.Errorf("save providers: provider %d is invalid", draft.ID)
		}
		if draft.ID != 0 {
			if _, ok := ids[draft.ID]; ok {
				return fmt.Errorf("save providers: provider %d is duplicated", draft.ID)
			}
			ids[draft.ID] = struct{}{}
		}
		if err := ValidateProviderDraft(draft); err != nil {
			return fmt.Errorf("save providers: provider %d: %w", index, err)
		}
		name := strings.ToLower(strings.TrimSpace(draft.Name))
		if _, ok := names[name]; ok {
			return fmt.Errorf("save providers: name %q is duplicated", draft.Name)
		}
		names[name] = struct{}{}
	}
	return nil
}

func ownedProviderIDs(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, userID int64) (map[int64]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM providers WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("list owned providers: %w", err)
	}
	defer rows.Close()
	ids := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owned provider: %w", err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned providers: %w", err)
	}
	return ids, nil
}

func writeProviderFields(ctx context.Context, db settingWriter, userID, id int64, endpoint string, draft ProviderDraft) error {
	sealedEndpoint, err := encryptProviderText(ctx, providerEndpointScope, userID, id, endpoint)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE providers SET endpoint = ? WHERE id = ? AND user_id = ?`, sealedEndpoint, id, userID); err != nil {
		return fmt.Errorf("store provider %d endpoint: %w", id, err)
	}

	switch draft.APIKeyChange {
	case LLMKeepAPIKey:
	case LLMReplaceAPIKey:
		apiKey := strings.TrimSpace(draft.APIKeyValue)
		if apiKey == "" {
			// A blank submitted key means keep the stored secret.
			break
		}
		sealedKey, err := encryptProviderText(ctx, providerAPIKeyScope, userID, id, apiKey)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `UPDATE providers SET api_key = ? WHERE id = ? AND user_id = ?`, sealedKey, id, userID); err != nil {
			return fmt.Errorf("store provider %d API key: %w", id, err)
		}
	case LLMClearAPIKey:
		if _, err := db.ExecContext(ctx, `UPDATE providers SET api_key = NULL WHERE id = ? AND user_id = ?`, id, userID); err != nil {
			return fmt.Errorf("clear provider %d API key: %w", id, err)
		}
	default:
		return fmt.Errorf("save providers: unknown API key change mode %d", draft.APIKeyChange)
	}
	return nil
}

func encryptProviderText(ctx context.Context, scope string, userID, providerID int64, value string) (string, error) {
	sealed, err := EncryptStorageText(ctx, scope, providerStorageRowID(providerID), value)
	if err != nil {
		return "", fmt.Errorf("encrypt %s: %w", scope, err)
	}
	return sealed, nil
}
