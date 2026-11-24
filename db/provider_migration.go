package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// MigrateLegacyProviderConfig converts each account's single legacy LLM
// endpoint and key into a provider named "Default" and moves existing model
// selections onto it. It is idempotent and transactional per account. The
// context must already carry the storage cipher so legacy values decrypt with
// the same binding they were written with.
func MigrateLegacyProviderConfig(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT id
		FROM settings
		WHERE llm_endpoint IS NOT NULL AND trim(llm_endpoint) <> ''
		ORDER BY id
	`)
	if err != nil {
		return fmt.Errorf("list legacy provider settings: %w", err)
	}
	userIDs := []int64{}
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy provider settings: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy provider settings: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy provider settings: %w", err)
	}

	for _, userID := range userIDs {
		userCtx := WithUser(ctx, userID)
		if err := migrateLegacyProviderForUser(userCtx, db); err != nil {
			return fmt.Errorf("migrate legacy provider for user %d: %w", userID, err)
		}
	}
	return nil
}

func migrateLegacyProviderForUser(ctx context.Context, db *sql.DB) error {
	userID, err := requireUserID(ctx)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy provider migration: %w", err)
	}
	defer tx.Rollback()

	var providerCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM providers WHERE user_id = ?`, userID).Scan(&providerCount); err != nil {
		return fmt.Errorf("count existing providers: %w", err)
	}
	if providerCount > 0 {
		return nil
	}

	config, err := getLLMConfig(ctx, tx)
	if err != nil {
		return fmt.Errorf("read legacy LLM config: %w", err)
	}
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		return nil
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO providers (user_id, position, name, endpoint, api_key)
		VALUES (?, 0, 'Default', 'pending', NULL)
	`, userID)
	if err != nil {
		return fmt.Errorf("insert default provider: %w", err)
	}
	providerID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get default provider ID: %w", err)
	}
	sealedEndpoint, err := encryptProviderText(ctx, providerEndpointScope, userID, providerID, endpoint)
	if err != nil {
		return err
	}
	var sealedKey any
	if apiKey := strings.TrimSpace(config.APIKey); apiKey != "" {
		sealed, err := encryptProviderText(ctx, providerAPIKeyScope, userID, providerID, apiKey)
		if err != nil {
			return err
		}
		sealedKey = sealed
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE providers SET endpoint = ?, api_key = ? WHERE id = ? AND user_id = ?
	`, sealedEndpoint, sealedKey, providerID, userID); err != nil {
		return fmt.Errorf("store default provider: %w", err)
	}

	if err := assignLegacyProviderReferences(ctx, tx, userID, providerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE settings SET llm_endpoint = NULL, llm_api_key = NULL WHERE id = ?
	`, userID); err != nil {
		return fmt.Errorf("clear legacy LLM settings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy provider migration: %w", err)
	}
	return nil
}

func assignLegacyProviderReferences(ctx context.Context, tx *sql.Tx, userID, providerID int64) error {
	for _, assignment := range []struct {
		column string
		model  string
	}{
		{"default_provider_id", "default_model"},
		{"title_provider_id", "title_generation_model"},
		{"tool_result_elision_summary_provider_id", "tool_result_elision_summary_model"},
	} {
		if _, err := tx.ExecContext(ctx, `
			UPDATE settings
			SET `+assignment.column+` = ?
			WHERE id = ?
				AND `+assignment.column+` IS NULL
				AND `+assignment.model+` IS NOT NULL
				AND trim(`+assignment.model+`) <> ''
		`, providerID, userID); err != nil {
			return fmt.Errorf("assign %s: %w", assignment.column, err)
		}
	}

	for _, statement := range []string{
		`UPDATE chats
		 SET provider_id = ?
		 WHERE user_id = ? AND provider_id IS NULL AND model IS NOT NULL AND trim(model) <> ''`,
		`UPDATE inbox_items
		 SET provider_id = ?
		 WHERE provider_id IS NULL AND model <> ''
			AND chat_id IN (SELECT id FROM chats WHERE user_id = ?)`,
		`UPDATE completion_state
		 SET provider_id = ?
		 WHERE provider_id IS NULL AND model <> ''
			AND chat_id IN (SELECT id FROM chats WHERE user_id = ?)`,
	} {
		if _, err := tx.ExecContext(ctx, statement, providerID, userID); err != nil {
			return fmt.Errorf("assign legacy provider to rows: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE model_endpoint_preferences
		SET provider_id = ?
		WHERE user_id = ? AND provider_id = 0
	`, providerID, userID); err != nil {
		return fmt.Errorf("assign legacy endpoint preferences: %w", err)
	}
	return nil
}
