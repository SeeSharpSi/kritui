package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
)

func TestChatSummaryTracksVisibleHistory(t *testing.T) {
	database := openTestDatabase(t)
	chatID := seedChat(t, database, "<script>title</script>", nil, nil)
	messages := []llm.Message{
		{Role: "user", Content: "Question"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Type: "function", Function: llm.FunctionCall{Name: "webfetch", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "call", Content: "Result"},
		{Role: "assistant", Content: "Answer"},
	}
	for position, message := range messages {
		if _, err := kritui_db.InsertMessage(t.Context(), database, chatID, position, message); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /chats/{chat}/summary", chatSummaryHandler(database))
	check := func(count string) {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/chats/"+strconv.FormatInt(chatID, 10)+"/summary", nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("summary status %d, headers %v", response.Code, response.Header())
		}
		requireContains(t, response.Body.String(), "&lt;script&gt;title&lt;/script&gt;", count+" messages")
		requireNotContains(t, response.Body.String(), "<script>")
	}
	check("2")
	if _, err := kritui_db.UndoLatestTurn(t.Context(), database, chatID); err != nil {
		t.Fatal(err)
	}
	check("0")
	if _, err := kritui_db.RedoLatestTurn(t.Context(), database, chatID); err != nil {
		t.Fatal(err)
	}
	check("2")

	for _, id := range []string{"0", "-1", "invalid"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/chats/"+id+"/summary", nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid chat %s returned %d", id, response.Code)
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/chats/999/summary", nil))
	requireContains(t, response.Body.String(), "New chat", "0 messages")
}

func TestMatteBlackMigrationPreservesExistingSettings(t *testing.T) {
	preserved := []string{"", "rose-pine", "rose-pine-dark", "nord", "forest-night"}
	remapped := map[string]string{"og": "rose-pine-dark", "omarchy": "matte-black"}
	savedThemes := append(append([]string{}, preserved...), "og", "omarchy")
	for _, savedTheme := range savedThemes {
		t.Run("theme="+savedTheme, func(t *testing.T) {
			database, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			database.SetMaxOpenConns(1)
			previousSchema := strings.Replace(schema,
				"'rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975'",
				"'rose-pine', 'rose-pine-dark', 'nord', 'tokyo-night', 'og', 'forest-night', 'omarchy'", 1)
			previousSchema = strings.Replace(previousSchema, "user_version = 21", "user_version = 18", 1)
			if _, err := database.Exec(previousSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = NULLIF(?, ''), default_model = 'kept-model', max_tool_rounds = 9,
				default_tools_configured = 1, prompt_appends_configured = 1,
				ntfy_endpoint = 'https://ntfy.example', ntfy_topic = 'kept-topic', ntfy_api_key = 'kept-key' WHERE id = 1`, savedTheme); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = 'matte-black' WHERE id = 1`); err == nil {
				t.Fatal("version 18 unexpectedly accepts the new theme")
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatal(err)
			}
			var theme sql.NullString
			var model, endpoint, topic, key string
			var rounds, defaultTools, appends int
			if err := database.QueryRow(`SELECT theme, default_model, max_tool_rounds, default_tools_configured,
				prompt_appends_configured, ntfy_endpoint, ntfy_topic, ntfy_api_key FROM settings WHERE id = 1`).Scan(
				&theme, &model, &rounds, &defaultTools, &appends, &endpoint, &topic, &key); err != nil {
				t.Fatal(err)
			}
			wantTheme := savedTheme
			if mapped, ok := remapped[savedTheme]; ok {
				wantTheme = mapped
			}
			if theme.String != wantTheme || theme.Valid != (wantTheme != "") || model != "kept-model" || rounds != 9 ||
				defaultTools != 1 || appends != 1 || endpoint != "https://ntfy.example" || topic != "kept-topic" || key != "kept-key" {
				t.Fatalf("migration produced theme %q, want %q (other settings must be preserved)", theme.String, wantTheme)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = 'matte-black' WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
			for _, removed := range []string{"og", "omarchy", "tokyo-night"} {
				if _, err := database.Exec(`UPDATE settings SET theme = ? WHERE id = 1`, removed); err == nil {
					t.Fatalf("migrated schema unexpectedly accepts removed theme %q", removed)
				}
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatalf("repeated migration: %v", err)
			}
		})
	}
}

func Test1975MigrationPreservesExistingSettings(t *testing.T) {
	preserved := []string{"", "rose-pine", "rose-pine-dark", "nord", "forest-night", "matte-black"}
	for _, savedTheme := range preserved {
		t.Run("theme="+savedTheme, func(t *testing.T) {
			database, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			database.SetMaxOpenConns(1)
			previousSchema := strings.Replace(schema,
				"'rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975'",
				"'rose-pine', 'rose-pine-dark', 'nord', 'tokyo-night', 'forest-night', 'matte-black'", 1)
			previousSchema = strings.Replace(previousSchema, "user_version = 21", "user_version = 19", 1)
			if _, err := database.Exec(previousSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = NULLIF(?, ''), default_model = 'kept-model', max_tool_rounds = 9,
				default_tools_configured = 1, prompt_appends_configured = 1,
				ntfy_endpoint = 'https://ntfy.example', ntfy_topic = 'kept-topic', ntfy_api_key = 'kept-key' WHERE id = 1`, savedTheme); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = '1975' WHERE id = 1`); err == nil {
				t.Fatal("version 19 unexpectedly accepts the new theme")
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatal(err)
			}
			var theme sql.NullString
			var model, endpoint, topic, key string
			var rounds, defaultTools, appends int
			if err := database.QueryRow(`SELECT theme, default_model, max_tool_rounds, default_tools_configured,
				prompt_appends_configured, ntfy_endpoint, ntfy_topic, ntfy_api_key FROM settings WHERE id = 1`).Scan(
				&theme, &model, &rounds, &defaultTools, &appends, &endpoint, &topic, &key); err != nil {
				t.Fatal(err)
			}
			if theme.String != savedTheme || theme.Valid != (savedTheme != "") || model != "kept-model" || rounds != 9 ||
				defaultTools != 1 || appends != 1 || endpoint != "https://ntfy.example" || topic != "kept-topic" || key != "kept-key" {
				t.Fatalf("migration produced theme %q, want %q (other settings must be preserved)", theme.String, savedTheme)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = '1975' WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatalf("repeated migration: %v", err)
			}
		})
	}
}

func TestTokyoNightRemovalRemapsToMatteBlack(t *testing.T) {
	preserved := []string{"", "rose-pine", "rose-pine-dark", "nord", "forest-night", "matte-black", "1975"}
	savedThemes := append(append([]string{}, preserved...), "tokyo-night")
	for _, savedTheme := range savedThemes {
		t.Run("theme="+savedTheme, func(t *testing.T) {
			database, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			database.SetMaxOpenConns(1)
			previousSchema := strings.Replace(schema,
				"'rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975'",
				"'rose-pine', 'rose-pine-dark', 'nord', 'tokyo-night', 'forest-night', 'matte-black', '1975'", 1)
			previousSchema = strings.Replace(previousSchema, "user_version = 21", "user_version = 20", 1)
			if _, err := database.Exec(previousSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = NULLIF(?, ''), default_model = 'kept-model', max_tool_rounds = 9,
				default_tools_configured = 1, prompt_appends_configured = 1,
				ntfy_endpoint = 'https://ntfy.example', ntfy_topic = 'kept-topic', ntfy_api_key = 'kept-key' WHERE id = 1`, savedTheme); err != nil {
				t.Fatal(err)
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatal(err)
			}
			var theme sql.NullString
			var model, endpoint, topic, key string
			var rounds, defaultTools, appends int
			if err := database.QueryRow(`SELECT theme, default_model, max_tool_rounds, default_tools_configured,
				prompt_appends_configured, ntfy_endpoint, ntfy_topic, ntfy_api_key FROM settings WHERE id = 1`).Scan(
				&theme, &model, &rounds, &defaultTools, &appends, &endpoint, &topic, &key); err != nil {
				t.Fatal(err)
			}
			wantTheme := savedTheme
			if savedTheme == "tokyo-night" {
				wantTheme = "matte-black"
			}
			if theme.String != wantTheme || theme.Valid != (wantTheme != "") || model != "kept-model" || rounds != 9 ||
				defaultTools != 1 || appends != 1 || endpoint != "https://ntfy.example" || topic != "kept-topic" || key != "kept-key" {
				t.Fatalf("migration produced theme %q, want %q (other settings must be preserved)", theme.String, wantTheme)
			}
			if _, err := database.Exec(`UPDATE settings SET theme = 'tokyo-night' WHERE id = 1`); err == nil {
				t.Fatal("migrated schema unexpectedly accepts removed theme tokyo-night")
			}
			if err := migrateDatabase(database); err != nil {
				t.Fatalf("repeated migration: %v", err)
			}
		})
	}
}

func TestSettingsCanSaveMatteBlackAndReturnToExistingTheme(t *testing.T) {
	database := openTestDatabase(t)
	for _, theme := range []string{"matte-black", "rose-pine", "matte-black"} {
		response := postForm(t, settingsHandler(database, newTestToolRegistry(t)), "/settings?chat=8", url.Values{
			"model": {"preview-model"}, "max_tool_rounds": {"16"}, "theme": {theme},
		})
		if response.Code != http.StatusOK {
			t.Fatalf("save %s returned %d", theme, response.Code)
		}
		stored, err := kritui_db.GetTheme(context.Background(), database)
		if err != nil || stored != theme {
			t.Fatalf("stored theme = %q, error = %v, want %s", stored, err, theme)
		}
		requireContains(t, response.Body.String(), `data-theme-id="`+theme+`"`, `value="`+theme+`" selected`)
	}
}

func TestHistoryCountExcludesEmptyAllocations(t *testing.T) {
	database := openTestDatabase(t)
	empty := seedChat(t, database, "Unsaved", nil, nil)
	saved := seedChat(t, database, "Saved", nil, nil)
	if _, err := kritui_db.InsertMessage(t.Context(), database, saved, 0, llm.Message{Role: "user", Content: "Question"}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	historyHandler(database)(response, httptest.NewRequest(http.MethodGet, "/history?chat="+strconv.FormatInt(empty, 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("history status = %d", response.Code)
	}
	requireContains(t, response.Body.String(), `id="history-count"`, `hx-swap-oob="outerHTML"`, "Browse · 1 chat", `id="history-summary-count"`)
	requireNotContains(t, response.Body.String(), "Unsaved")
	if _, err := database.Exec(`DELETE FROM chats WHERE id = ?`, saved); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	historyHandler(database)(response, httptest.NewRequest(http.MethodGet, "/history?chat="+strconv.FormatInt(empty, 10), nil))
	requireContains(t, response.Body.String(), "No saved chats yet.", "Browse · 0 chats")
}
