package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/themes"
)

const MaxConfigurableToolRounds = 100

// settingWriter is satisfied by both *sql.DB and *sql.Tx so setting writes can
// run inside a shared transaction.
type settingWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// SettingsUpdate describes the desired settings for one atomic save.
// Nil PromptAppends, MCPServers, Ntfy, LLM, or ToolResultElision values, and
// an empty Theme, leave those settings untouched.
type SettingsUpdate struct {
	Model             string
	MaxToolRounds     int
	DefaultTools      []string
	PromptAppends     []PromptAppend
	MCPServers        []MCPServerUpdate
	Ntfy              *NtfySettingsUpdate
	LLM               *LLMSettingsUpdate
	ToolResultElision *llm.ToolResultElisionConfig
	Theme             string
}

// NtfySettings contains values safe to render in the settings page.
// APIKeyConfigured reports secret presence without exposing the secret.
type NtfySettings struct {
	Endpoint         string
	Topic            string
	APIKeyConfigured bool
}

// NtfyPublishConfig contains credentials needed for one notification.
// Keep this type out of template and HTTP response data.
type NtfyPublishConfig struct {
	Endpoint string
	Topic    string
	APIKey   string
}

// NtfyAPIKeyChange selects how SaveNtfySettings treats the stored API key.
// The zero value preserves the existing secret.
type NtfyAPIKeyChange uint8

const (
	// NtfyKeepAPIKey leaves the stored key untouched.
	NtfyKeepAPIKey NtfyAPIKeyChange = iota
	// NtfyReplaceAPIKey stores Update.APIKeyValue, or NULL when it trims empty.
	NtfyReplaceAPIKey
	// NtfyClearAPIKey removes the stored key.
	NtfyClearAPIKey
)

// NtfySettingsUpdate changes notification settings. APIKeyChange and
// APIKeyValue form an exhaustive tri-state decision about the stored secret,
// so contradictory instructions are unrepresentable.
type NtfySettingsUpdate struct {
	Endpoint     string
	Topic        string
	APIKeyChange NtfyAPIKeyChange
	APIKeyValue  string
}

// LLMSettings contains values safe to render in the settings page.
// APIKeyConfigured reports secret presence without exposing the secret.
type LLMSettings struct {
	Endpoint         string
	APIKeyConfigured bool
}

// LLMConfig contains credentials needed for model completion.
// Keep this type out of template and HTTP response data.
type LLMConfig struct {
	Endpoint string
	APIKey   string
}

// LLMAPIKeyChange selects how SaveLLMSettings treats the stored API key.
// The zero value preserves the existing secret.
type LLMAPIKeyChange uint8

const (
	// LLMKeepAPIKey leaves the stored key untouched.
	LLMKeepAPIKey LLMAPIKeyChange = iota
	// LLMReplaceAPIKey stores Update.APIKeyValue, or NULL when it trims empty.
	LLMReplaceAPIKey
	// LLMClearAPIKey removes the stored key.
	LLMClearAPIKey
)

// LLMSettingsUpdate changes LLM settings. APIKeyChange and APIKeyValue form
// an exhaustive tri-state decision about the stored secret, so contradictory
// instructions are unrepresentable.
type LLMSettingsUpdate struct {
	Endpoint     string
	APIKeyChange LLMAPIKeyChange
	APIKeyValue  string
}

// SaveSettings stores all submitted settings in one transaction.
func SaveSettings(ctx context.Context, db *sql.DB, update SettingsUpdate) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set settings: %w", err)
	}
	defer tx.Rollback()

	if err := setDefaultModel(ctx, tx, update.Model); err != nil {
		return err
	}
	if err := setMaxToolRounds(ctx, tx, update.MaxToolRounds); err != nil {
		return err
	}
	if err := setDefaultEnabledTools(ctx, tx, update.DefaultTools); err != nil {
		return err
	}
	if update.PromptAppends != nil {
		if err := setPromptAppends(ctx, tx, update.PromptAppends); err != nil {
			return err
		}
	}
	if update.MCPServers != nil {
		if err := setMCPServers(ctx, tx, update.MCPServers); err != nil {
			return err
		}
	}
	if update.Ntfy != nil {
		if err := setNtfySettings(ctx, tx, *update.Ntfy); err != nil {
			return err
		}
	}
	if update.LLM != nil {
		if err := setLLMSettings(ctx, tx, *update.LLM); err != nil {
			return err
		}
	}
	if update.ToolResultElision != nil {
		if err := setToolResultElision(ctx, tx, *update.ToolResultElision); err != nil {
			return err
		}
	}
	if strings.TrimSpace(update.Theme) != "" {
		if err := setTheme(ctx, tx, update.Theme); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set settings: %w", err)
	}
	return nil
}

// GetToolResultElision returns the stored tool-result elision configuration.
func GetToolResultElision(ctx context.Context, db *sql.DB) (llm.ToolResultElisionConfig, error) {
	var mode string
	var userTurns, tokenBudget sql.NullInt64
	var summaryModel sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT tool_result_elision_mode, tool_result_elision_user_turns,
			tool_result_elision_token_budget, tool_result_elision_summary_model
		FROM settings
		WHERE id = 1
	`).Scan(&mode, &userTurns, &tokenBudget, &summaryModel); err != nil {
		return llm.ToolResultElisionConfig{}, fmt.Errorf("get tool result elision: %w", err)
	}

	config := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionMode(mode)}
	switch config.Mode {
	case llm.ToolResultElisionLastUserTurns:
		if userTurns.Valid {
			config.UserTurns = int(userTurns.Int64)
		}
	case llm.ToolResultElisionBudget:
		if tokenBudget.Valid {
			config.TokenBudget = int(tokenBudget.Int64)
		}
	case llm.ToolResultElisionSummarize:
		if summaryModel.Valid {
			config.SummaryModel = strings.TrimSpace(summaryModel.String)
		}
	}
	if err := config.Validate(); err != nil {
		return llm.ToolResultElisionConfig{}, fmt.Errorf("get tool result elision: %w", err)
	}
	return config, nil
}

func setToolResultElision(ctx context.Context, db settingWriter, config llm.ToolResultElisionConfig) error {
	if config.Mode == "" {
		config.Mode = llm.ToolResultElisionNone
	}
	config.SummaryModel = strings.TrimSpace(config.SummaryModel)
	if err := config.Validate(); err != nil {
		return fmt.Errorf("set tool result elision: %w", err)
	}

	var userTurns, tokenBudget, summaryModel any
	switch config.Mode {
	case llm.ToolResultElisionLastUserTurns:
		userTurns = config.UserTurns
	case llm.ToolResultElisionBudget:
		tokenBudget = config.TokenBudget
	case llm.ToolResultElisionSummarize:
		summaryModel = config.SummaryModel
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE settings
		SET tool_result_elision_mode = ?,
			tool_result_elision_user_turns = ?,
			tool_result_elision_token_budget = ?,
			tool_result_elision_summary_model = ?
		WHERE id = 1
	`, string(config.Mode), userTurns, tokenBudget, summaryModel); err != nil {
		return fmt.Errorf("set tool result elision: %w", err)
	}
	return nil
}

// GetTheme returns the stored theme slug or an empty string when unconfigured.
func GetTheme(ctx context.Context, db *sql.DB) (string, error) {
	var theme sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT theme FROM settings WHERE id = 1`).Scan(&theme); err != nil {
		return "", fmt.Errorf("get theme: %w", err)
	}
	if !theme.Valid {
		return "", nil
	}
	return strings.TrimSpace(theme.String), nil
}

func setTheme(ctx context.Context, db settingWriter, theme string) error {
	theme = strings.TrimSpace(theme)
	if theme == "" {
		return fmt.Errorf("set theme: theme is required")
	}
	if _, err := themes.ByID(theme); err != nil {
		return fmt.Errorf("set theme: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET theme = ? WHERE id = 1`, theme); err != nil {
		return fmt.Errorf("set theme: %w", err)
	}
	return nil
}

// GetNtfySettings returns notification values safe for frontend rendering.
func GetNtfySettings(ctx context.Context, db *sql.DB) (NtfySettings, error) {
	var endpoint, topic sql.NullString
	var apiKeyConfigured bool
	if err := db.QueryRowContext(ctx, `
		SELECT ntfy_endpoint, ntfy_topic,
			CASE WHEN ntfy_api_key IS NOT NULL AND trim(ntfy_api_key) <> '' THEN 1 ELSE 0 END
		FROM settings
		WHERE id = 1
	`).Scan(&endpoint, &topic, &apiKeyConfigured); err != nil {
		return NtfySettings{}, fmt.Errorf("get ntfy settings: %w", err)
	}
	return NtfySettings{
		Endpoint:         nullSettingString(endpoint),
		Topic:            nullSettingString(topic),
		APIKeyConfigured: apiKeyConfigured,
	}, nil
}

// GetNtfyPublishConfig returns notification credentials for backend use.
func GetNtfyPublishConfig(ctx context.Context, db *sql.DB) (NtfyPublishConfig, error) {
	config, err := getNtfyPublishConfig(ctx, db)
	if err != nil {
		return NtfyPublishConfig{}, fmt.Errorf("get ntfy publish config: %w", err)
	}
	return config, nil
}

type settingReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getNtfyPublishConfig(ctx context.Context, db settingReader) (NtfyPublishConfig, error) {
	var endpoint, topic, apiKey sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT ntfy_endpoint, ntfy_topic, ntfy_api_key
		FROM settings
		WHERE id = 1
	`).Scan(&endpoint, &topic, &apiKey); err != nil {
		return NtfyPublishConfig{}, err
	}
	return NtfyPublishConfig{
		Endpoint: nullSettingString(endpoint),
		Topic:    nullSettingString(topic),
		APIKey:   nullSettingString(apiKey),
	}, nil
}

func nullSettingString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

// SaveNtfySettings atomically updates notification destination and secret.
func SaveNtfySettings(ctx context.Context, db *sql.DB, update NtfySettingsUpdate) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set ntfy settings: %w", err)
	}
	defer tx.Rollback()

	if err := setNtfySettings(ctx, tx, update); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set ntfy settings: %w", err)
	}
	return nil
}

func setNtfySettings(ctx context.Context, db settingWriter, update NtfySettingsUpdate) error {
	endpoint := strings.TrimSpace(update.Endpoint)
	topic := strings.TrimSpace(update.Topic)
	if (endpoint == "") != (topic == "") {
		return fmt.Errorf("set ntfy settings: endpoint and topic must both be set or empty")
	}

	if endpoint == "" {
		if _, err := db.ExecContext(ctx, `
			UPDATE settings
			SET ntfy_endpoint = NULL, ntfy_topic = NULL, ntfy_api_key = NULL
			WHERE id = 1
		`); err != nil {
			return fmt.Errorf("clear ntfy settings: %w", err)
		}
	} else {
		if _, err := db.ExecContext(ctx, `
			UPDATE settings
			SET ntfy_endpoint = ?, ntfy_topic = ?
			WHERE id = 1
		`, endpoint, topic); err != nil {
			return fmt.Errorf("set ntfy destination: %w", err)
		}

		switch update.APIKeyChange {
		case NtfyKeepAPIKey:
		case NtfyReplaceAPIKey:
			apiKey := strings.TrimSpace(update.APIKeyValue)
			var value any
			if apiKey != "" {
				value = apiKey
			}
			if _, err := db.ExecContext(ctx, `UPDATE settings SET ntfy_api_key = ? WHERE id = 1`, value); err != nil {
				return fmt.Errorf("set ntfy API key: %w", err)
			}
		case NtfyClearAPIKey:
			if _, err := db.ExecContext(ctx, `UPDATE settings SET ntfy_api_key = NULL WHERE id = 1`); err != nil {
				return fmt.Errorf("clear ntfy API key: %w", err)
			}
		default:
			return fmt.Errorf("set ntfy settings: unknown API key change mode %d", update.APIKeyChange)
		}
	}
	return nil
}

// GetLLMSettings returns LLM values safe for frontend rendering.
func GetLLMSettings(ctx context.Context, db *sql.DB) (LLMSettings, error) {
	var endpoint sql.NullString
	var apiKeyConfigured bool
	if err := db.QueryRowContext(ctx, `
		SELECT llm_endpoint,
			CASE WHEN llm_api_key IS NOT NULL AND trim(llm_api_key) <> '' THEN 1 ELSE 0 END
		FROM settings
		WHERE id = 1
	`).Scan(&endpoint, &apiKeyConfigured); err != nil {
		return LLMSettings{}, fmt.Errorf("get llm settings: %w", err)
	}
	return LLMSettings{
		Endpoint:         nullSettingString(endpoint),
		APIKeyConfigured: apiKeyConfigured,
	}, nil
}

// GetLLMConfig returns LLM credentials for backend use.
func GetLLMConfig(ctx context.Context, db *sql.DB) (LLMConfig, error) {
	config, err := getLLMConfig(ctx, db)
	if err != nil {
		return LLMConfig{}, fmt.Errorf("get llm config: %w", err)
	}
	return config, nil
}

func getLLMConfig(ctx context.Context, db settingReader) (LLMConfig, error) {
	var endpoint, apiKey sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT llm_endpoint, llm_api_key
		FROM settings
		WHERE id = 1
	`).Scan(&endpoint, &apiKey); err != nil {
		return LLMConfig{}, err
	}
	return LLMConfig{
		Endpoint: nullSettingString(endpoint),
		APIKey:   nullSettingString(apiKey),
	}, nil
}

// SaveLLMSettings atomically updates LLM endpoint and secret.
func SaveLLMSettings(ctx context.Context, db *sql.DB, update LLMSettingsUpdate) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set llm settings: %w", err)
	}
	defer tx.Rollback()

	if err := setLLMSettings(ctx, tx, update); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set llm settings: %w", err)
	}
	return nil
}

// ValidateLLMSettings checks an LLM endpoint and key update without writes.
func ValidateLLMSettings(update LLMSettingsUpdate) error {
	endpoint := strings.TrimSpace(update.Endpoint)
	if len(endpoint) > 2048 {
		return fmt.Errorf("set llm settings: endpoint must not exceed 2048 bytes")
	}
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("set llm settings: parse endpoint: %w", err)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("set llm settings: endpoint must be an absolute HTTP or HTTPS URL")
		}
		if parsed.Fragment != "" {
			return fmt.Errorf("set llm settings: endpoint must not contain a fragment")
		}
	}
	switch update.APIKeyChange {
	case LLMKeepAPIKey:
	case LLMReplaceAPIKey:
		if len(strings.TrimSpace(update.APIKeyValue)) > 16384 {
			return fmt.Errorf("set llm settings: API key must not exceed 16384 bytes")
		}
	case LLMClearAPIKey:
	default:
		return fmt.Errorf("set llm settings: unknown API key change mode %d", update.APIKeyChange)
	}
	return nil
}

func setLLMSettings(ctx context.Context, db settingWriter, update LLMSettingsUpdate) error {
	if err := ValidateLLMSettings(update); err != nil {
		return err
	}
	endpoint := strings.TrimSpace(update.Endpoint)

	var endpointValue any
	if endpoint != "" {
		endpointValue = endpoint
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET llm_endpoint = ? WHERE id = 1`, endpointValue); err != nil {
		return fmt.Errorf("set llm endpoint: %w", err)
	}

	switch update.APIKeyChange {
	case LLMKeepAPIKey:
	case LLMReplaceAPIKey:
		apiKey := strings.TrimSpace(update.APIKeyValue)
		var value any
		if apiKey != "" {
			value = apiKey
		}
		if _, err := db.ExecContext(ctx, `UPDATE settings SET llm_api_key = ? WHERE id = 1`, value); err != nil {
			return fmt.Errorf("set llm API key: %w", err)
		}
	case LLMClearAPIKey:
		if _, err := db.ExecContext(ctx, `UPDATE settings SET llm_api_key = NULL WHERE id = 1`); err != nil {
			return fmt.Errorf("clear llm API key: %w", err)
		}
	default:
		return fmt.Errorf("set llm settings: unknown API key change mode %d", update.APIKeyChange)
	}
	return nil
}

// GetDefaultModel returns the stored default model or fallback when unset.
func GetDefaultModel(ctx context.Context, db *sql.DB, fallback string) (string, error) {
	var model sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT default_model FROM settings WHERE id = 1`).Scan(&model); err != nil {
		return "", fmt.Errorf("get default model: %w", err)
	}
	if !model.Valid {
		return strings.TrimSpace(fallback), nil
	}
	return strings.TrimSpace(model.String), nil
}

// EnsureDefaultModel stores model only when no default has been configured yet.
func EnsureDefaultModel(ctx context.Context, db *sql.DB, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE settings SET default_model = ?
		WHERE id = 1 AND default_model IS NULL
	`, model); err != nil {
		return fmt.Errorf("ensure default model: %w", err)
	}
	return nil
}

func setDefaultModel(ctx context.Context, db settingWriter, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("set default model: model is required")
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET default_model = ? WHERE id = 1`, model); err != nil {
		return fmt.Errorf("set default model: %w", err)
	}
	return nil
}

// GetMaxToolRounds returns the stored maximum consecutive tool-call rounds or
// fallback when unset.
func GetMaxToolRounds(ctx context.Context, db *sql.DB, fallback int) (int, error) {
	var rounds sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max_tool_rounds FROM settings WHERE id = 1`).Scan(&rounds); err != nil {
		return 0, fmt.Errorf("get max tool rounds: %w", err)
	}
	if !rounds.Valid {
		return fallback, nil
	}
	return int(rounds.Int64), nil
}

func setMaxToolRounds(ctx context.Context, db settingWriter, rounds int) error {
	if err := validateMaxToolRounds(rounds); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET max_tool_rounds = ? WHERE id = 1`, rounds); err != nil {
		return fmt.Errorf("set max tool rounds: %w", err)
	}
	return nil
}

func validateMaxToolRounds(rounds int) error {
	if rounds < 1 || rounds > MaxConfigurableToolRounds {
		return fmt.Errorf("set max tool rounds: rounds must be between 1 and %d", MaxConfigurableToolRounds)
	}
	return nil
}

// GetDefaultEnabledTools returns tools enabled by default or fallback when the
// collection has never been configured.
func GetDefaultEnabledTools(ctx context.Context, db *sql.DB, fallback []string) ([]string, error) {
	var configured bool
	if err := db.QueryRowContext(ctx, `SELECT default_tools_configured FROM settings WHERE id = 1`).Scan(&configured); err != nil {
		return nil, fmt.Errorf("get default enabled tools state: %w", err)
	}
	if !configured {
		return fallback, nil
	}

	rows, err := db.QueryContext(ctx, `SELECT name FROM default_tools ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("get default enabled tools: %w", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan default enabled tool: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate default enabled tools: %w", err)
	}
	return names, nil
}

func setDefaultEnabledTools(ctx context.Context, db settingWriter, names []string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM default_tools`); err != nil {
		return fmt.Errorf("clear default enabled tools: %w", err)
	}
	for position, name := range names {
		if _, err := db.ExecContext(ctx, `INSERT INTO default_tools (position, name) VALUES (?, ?)`, position, name); err != nil {
			return fmt.Errorf("store default enabled tool %d: %w", position, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET default_tools_configured = 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("mark default enabled tools configured: %w", err)
	}
	return nil
}
