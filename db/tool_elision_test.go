package kritui_db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestGetToolResultElisionDefaultsToNone(t *testing.T) {
	database := openMessagesTestDatabase(t, "")

	got, err := GetToolResultElision(context.Background(), database)
	if err != nil {
		t.Fatalf("GetToolResultElision() error: %v", err)
	}
	want := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone}
	if got != want {
		t.Errorf("tool result elision = %#v, want %#v", got, want)
	}
}

func TestSaveSettingsRoundTripsToolResultElisionModes(t *testing.T) {
	tests := []struct {
		name             string
		config           llm.ToolResultElisionConfig
		want             llm.ToolResultElisionConfig
		wantUserTurns    *int64
		wantTokenBudget  *int64
		wantSummaryModel *string
	}{
		{
			name:   "none",
			config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone, UserTurns: 9, TokenBudget: 99, SummaryModel: "unused"},
			want:   llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionNone},
		},
		{
			name:   "current turn",
			config: llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn, UserTurns: 9, TokenBudget: 99, SummaryModel: "unused"},
			want:   llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn},
		},
		{
			name:          "last user turns",
			config:        llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 17, TokenBudget: 99, SummaryModel: "unused"},
			want:          llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 17},
			wantUserTurns: int64Pointer(17),
		},
		{
			name:            "tool result budget",
			config:          llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, UserTurns: 9, TokenBudget: 4096, SummaryModel: "unused"},
			want:            llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 4096},
			wantTokenBudget: int64Pointer(4096),
		},
		{
			name:             "summarize trims model",
			config:           llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, UserTurns: 9, TokenBudget: 99, SummaryModel: "  summary-model  "},
			want:             llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: "summary-model"},
			wantSummaryModel: stringPointer("summary-model"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openMessagesTestDatabase(t, "")
			if err := SaveSettings(context.Background(), database, SettingsUpdate{
				Model:             "test-model",
				MaxToolRounds:     4,
				ToolResultElision: &test.config,
			}); err != nil {
				t.Fatalf("SaveSettings() error: %v", err)
			}

			got, err := GetToolResultElision(context.Background(), database)
			if err != nil {
				t.Fatalf("GetToolResultElision() error: %v", err)
			}
			if got != test.want {
				t.Errorf("tool result elision = %#v, want %#v", got, test.want)
			}

			var mode string
			var userTurns, tokenBudget sql.NullInt64
			var summaryModel sql.NullString
			if err := database.QueryRow(`
				SELECT tool_result_elision_mode, tool_result_elision_user_turns,
					tool_result_elision_token_budget, tool_result_elision_summary_model
				FROM settings WHERE id = 1
			`).Scan(&mode, &userTurns, &tokenBudget, &summaryModel); err != nil {
				t.Fatalf("query stored tool result elision: %v", err)
			}
			if mode != string(test.want.Mode) {
				t.Errorf("stored mode = %q, want %q", mode, test.want.Mode)
			}
			assertNullableInt64(t, "user turns", userTurns, test.wantUserTurns)
			assertNullableInt64(t, "token budget", tokenBudget, test.wantTokenBudget)
			assertNullableString(t, "summary model", summaryModel, test.wantSummaryModel)
		})
	}
}

func TestSaveSettingsNilToolResultElisionPreservesConfigAndLLMSecrets(t *testing.T) {
	ctx := context.Background()
	database := openMessagesTestDatabase(t, "")
	const endpoint = "https://llm.example/v1/responses"
	const apiKey = "stored-llm-secret"
	if err := SaveLLMSettings(ctx, database, LLMSettingsUpdate{
		Endpoint:     endpoint,
		APIKeyChange: LLMReplaceAPIKey,
		APIKeyValue:  apiKey,
	}); err != nil {
		t.Fatalf("seed LLM settings: %v", err)
	}

	wantElision := llm.ToolResultElisionConfig{
		Mode:        llm.ToolResultElisionBudget,
		TokenBudget: 2048,
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:             "first-model",
		MaxToolRounds:     4,
		ToolResultElision: &wantElision,
	}); err != nil {
		t.Fatalf("SaveSettings() with elision config: %v", err)
	}
	if err := SaveSettings(ctx, database, SettingsUpdate{
		Model:         "second-model",
		MaxToolRounds: 5,
	}); err != nil {
		t.Fatalf("SaveSettings() with nil elision config: %v", err)
	}

	gotElision, err := GetToolResultElision(ctx, database)
	if err != nil {
		t.Fatalf("GetToolResultElision() error: %v", err)
	}
	if gotElision != wantElision {
		t.Errorf("tool result elision after omitted update = %#v, want %#v", gotElision, wantElision)
	}
	gotLLM, err := GetLLMConfig(ctx, database)
	if err != nil {
		t.Fatalf("GetLLMConfig() error: %v", err)
	}
	if gotLLM.Endpoint != endpoint || gotLLM.APIKey != apiKey {
		t.Errorf("LLM config after elision saves = %#v, want endpoint and preserved secret", gotLLM)
	}
}

func TestSaveSettingsRollsBackInvalidToolResultElision(t *testing.T) {
	invalid := []struct {
		name   string
		config llm.ToolResultElisionConfig
	}{
		{"user turns below range", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 0}},
		{"user turns above range", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionLastUserTurns, UserTurns: llm.MaxElisionUserTurns + 1}},
		{"token budget below range", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: 0}},
		{"token budget above range", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionBudget, TokenBudget: llm.MaxElisionTokenBudget + 1}},
		{"blank summary model", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: " \t "}},
		{"long summary model", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionSummarize, SummaryModel: strings.Repeat("m", 513)}},
		{"unknown mode", llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionMode("unknown")}},
	}

	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database := openMessagesTestDatabase(t, "")
			oldElision := llm.ToolResultElisionConfig{Mode: llm.ToolResultElisionCurrentTurn}
			if err := SaveSettings(ctx, database, SettingsUpdate{
				Model:             "old-model",
				MaxToolRounds:     3,
				DefaultTools:      []string{"git"},
				ToolResultElision: &oldElision,
			}); err != nil {
				t.Fatalf("seed SaveSettings(): %v", err)
			}

			if err := SaveSettings(ctx, database, SettingsUpdate{
				Model:             "new-model",
				MaxToolRounds:     9,
				DefaultTools:      []string{"webfetch"},
				ToolResultElision: &test.config,
			}); err == nil {
				t.Fatal("SaveSettings() error = nil, want invalid elision config error")
			}

			if got, err := GetDefaultModel(ctx, database, "fallback"); err != nil {
				t.Fatalf("GetDefaultModel() after rollback: %v", err)
			} else if got != "old-model" {
				t.Errorf("default model after rollback = %q, want old-model", got)
			}
			if got, err := GetMaxToolRounds(ctx, database, 1); err != nil {
				t.Fatalf("GetMaxToolRounds() after rollback: %v", err)
			} else if got != 3 {
				t.Errorf("max tool rounds after rollback = %d, want 3", got)
			}
			if got, err := GetDefaultEnabledTools(ctx, database, nil); err != nil {
				t.Fatalf("GetDefaultEnabledTools() after rollback: %v", err)
			} else if len(got) != 1 || got[0] != "git" {
				t.Errorf("default tools after rollback = %v, want [git]", got)
			}
			if got, err := GetToolResultElision(ctx, database); err != nil {
				t.Fatalf("GetToolResultElision() after rollback: %v", err)
			} else if got != oldElision {
				t.Errorf("tool result elision after rollback = %#v, want %#v", got, oldElision)
			}
		})
	}
}

func TestToolResultElisionColumnsRejectInvalidValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	invalid := []struct {
		name  string
		query string
		value any
	}{
		{"unknown mode", `UPDATE settings SET tool_result_elision_mode = ? WHERE id = 1`, "discard"},
		{"user turns below range", `UPDATE settings SET tool_result_elision_user_turns = ? WHERE id = 1`, 0},
		{"user turns above range", `UPDATE settings SET tool_result_elision_user_turns = ? WHERE id = 1`, llm.MaxElisionUserTurns + 1},
		{"token budget below range", `UPDATE settings SET tool_result_elision_token_budget = ? WHERE id = 1`, 0},
		{"token budget above range", `UPDATE settings SET tool_result_elision_token_budget = ? WHERE id = 1`, llm.MaxElisionTokenBudget + 1},
		{"blank summary model", `UPDATE settings SET tool_result_elision_summary_model = ? WHERE id = 1`, ""},
		{"whitespace summary model", `UPDATE settings SET tool_result_elision_summary_model = ? WHERE id = 1`, "   "},
		{"long summary model", `UPDATE settings SET tool_result_elision_summary_model = ? WHERE id = 1`, strings.Repeat("m", 513)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := database.Exec(test.query, test.value); err == nil {
				t.Errorf("store invalid setting %v: error = nil", test.value)
			}
		})
	}

	if _, err := database.Exec(`
		UPDATE settings
		SET tool_result_elision_mode = 'current_turn',
			tool_result_elision_user_turns = NULL,
			tool_result_elision_token_budget = NULL,
			tool_result_elision_summary_model = NULL
		WHERE id = 1
	`); err != nil {
		t.Errorf("store mode with NULL inactive parameters: %v", err)
	}
}

func assertNullableInt64(t *testing.T, name string, got sql.NullInt64, want *int64) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("stored %s = %d, want NULL", name, got.Int64)
		}
		return
	}
	if !got.Valid || got.Int64 != *want {
		t.Errorf("stored %s = %#v, want %d", name, got, *want)
	}
}

func assertNullableString(t *testing.T, name string, got sql.NullString, want *string) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("stored %s = %q, want NULL", name, got.String)
		}
		return
	}
	if !got.Valid || got.String != *want {
		t.Errorf("stored %s = %#v, want %q", name, got, *want)
	}
}

func int64Pointer(value int64) *int64 { return &value }

func stringPointer(value string) *string { return &value }
