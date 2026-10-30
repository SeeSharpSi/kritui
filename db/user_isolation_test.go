package kritui_db

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"seesharpsi/kritui/llm"
)

func createIsolationUser(t *testing.T, database *sql.DB, username string) (User, context.Context) {
	t.Helper()
	user, err := CreateUser(context.Background(), database, username, "encoded-"+username)
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	return user, WithUser(context.Background(), user.ID)
}

func TestUserSettingsAreIsolated(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	err := SaveSettings(aliceCtx, database, SettingsUpdate{
		Model:         "alice-model",
		MaxToolRounds: 7,
		DefaultTools:  []string{"webfetch"},
		PromptAppends: []PromptAppend{{ID: "alice", Name: "Alice", Text: "Alice only."}},
		Ntfy: &NtfySettingsUpdate{
			Endpoint: "https://ntfy.example", Topic: "alice-topic",
			APIKeyChange: NtfyReplaceAPIKey, APIKeyValue: "alice-ntfy-secret",
		},
		LLM: &LLMSettingsUpdate{
			Endpoint:     "https://alice.example/v1/responses",
			APIKeyChange: LLMReplaceAPIKey, APIKeyValue: "alice-llm-secret",
		},
		ToolResultElision: &llm.ToolResultElisionConfig{
			Mode: llm.ToolResultElisionLastUserTurns, UserTurns: 4,
		},
		CompactionPrompt: strPtr("Alice compaction prompt."),
		Theme:            "nord",
	})
	if err != nil {
		t.Fatalf("save alice settings: %v", err)
	}

	if got, err := GetDefaultModel(aliceCtx, database, "fallback"); err != nil || got != "alice-model" {
		t.Fatalf("alice default model = %q, %v", got, err)
	}
	if got, err := GetDefaultModel(bobCtx, database, "fallback"); err != nil || got != "fallback" {
		t.Fatalf("bob default model = %q, %v, want fallback", got, err)
	}
	if got, err := GetMaxToolRounds(aliceCtx, database, 16); err != nil || got != 7 {
		t.Fatalf("alice max tool rounds = %d, %v", got, err)
	}
	if got, err := GetMaxToolRounds(bobCtx, database, 16); err != nil || got != 16 {
		t.Fatalf("bob max tool rounds = %d, %v, want fallback", got, err)
	}
	if got, err := GetDefaultEnabledTools(aliceCtx, database, nil); err != nil || !slices.Equal(got, []string{"webfetch"}) {
		t.Fatalf("alice default tools = %v, %v", got, err)
	}
	if got, err := GetDefaultEnabledTools(bobCtx, database, []string{"git"}); err != nil || !slices.Equal(got, []string{"git"}) {
		t.Fatalf("bob default tools = %v, %v, want fallback", got, err)
	}
	if got, err := GetPromptAppends(aliceCtx, database); err != nil || len(got) != 1 || got[0].ID != "alice" {
		t.Fatalf("alice prompt appends = %#v, %v", got, err)
	}
	if got, err := GetPromptAppends(bobCtx, database); err != nil || !slices.Equal(got, DefaultPromptAppends()) {
		t.Fatalf("bob prompt appends = %#v, %v, want built-in defaults", got, err)
	}

	aliceNtfy, err := GetNtfyPublishConfig(aliceCtx, database)
	if err != nil || aliceNtfy.APIKey != "alice-ntfy-secret" || aliceNtfy.Topic != "alice-topic" {
		t.Fatalf("alice ntfy config = %#v, %v", aliceNtfy, err)
	}
	bobNtfy, err := GetNtfySettings(bobCtx, database)
	if err != nil || bobNtfy.APIKeyConfigured || bobNtfy.Endpoint != "" || bobNtfy.Topic != "" {
		t.Fatalf("bob ntfy settings = %#v, %v, want empty", bobNtfy, err)
	}
	aliceLLM, err := GetLLMConfig(aliceCtx, database)
	if err != nil || aliceLLM.APIKey != "alice-llm-secret" || aliceLLM.Endpoint != "https://alice.example/v1/responses" {
		t.Fatalf("alice llm config = %#v, %v", aliceLLM, err)
	}
	bobLLM, err := GetLLMSettings(bobCtx, database)
	if err != nil || bobLLM.APIKeyConfigured || bobLLM.Endpoint != "" {
		t.Fatalf("bob llm settings = %#v, %v, want empty", bobLLM, err)
	}
	if got, err := GetTheme(aliceCtx, database); err != nil || got != "nord" {
		t.Fatalf("alice theme = %q, %v", got, err)
	}
	if got, err := GetTheme(bobCtx, database); err != nil || got != "" {
		t.Fatalf("bob theme = %q, %v, want empty", got, err)
	}
	aliceElision, err := GetToolResultElision(aliceCtx, database)
	if err != nil || aliceElision.Mode != llm.ToolResultElisionLastUserTurns || aliceElision.UserTurns != 4 {
		t.Fatalf("alice elision = %#v, %v", aliceElision, err)
	}
	bobElision, err := GetToolResultElision(bobCtx, database)
	if err != nil || bobElision.Mode != llm.ToolResultElisionNone {
		t.Fatalf("bob elision = %#v, %v, want none", bobElision, err)
	}

	var settingsRows, ownerSecrets, aliceSecrets int
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&settingsRows); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings WHERE id = ?`, LegacyOwnerID).Scan(&ownerSecrets); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings WHERE id = ? AND llm_api_key = 'alice-llm-secret'`, alice.ID).Scan(&aliceSecrets); err != nil {
		t.Fatal(err)
	}
	if settingsRows != 3 || ownerSecrets != 1 || aliceSecrets != 1 {
		t.Fatalf("settings rows=%d owner=%d alice secret=%d, want 3,1,1", settingsRows, ownerSecrets, aliceSecrets)
	}
}

func TestUserAppendAndMCPPruningIsScoped(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	appendValue := PromptAppend{ID: "shared", Name: "Shared", Text: "Shared text."}
	if err := setPromptAppends(aliceCtx, database, []PromptAppend{appendValue}); err != nil {
		t.Fatalf("set alice appends: %v", err)
	}
	if err := setPromptAppends(bobCtx, database, []PromptAppend{appendValue}); err != nil {
		t.Fatalf("set bob appends: %v", err)
	}
	aliceChat, err := AllocateChat(aliceCtx, database, nil, []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	bobChat, err := AllocateChat(bobCtx, database, nil, []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}

	if err := setPromptAppends(aliceCtx, database, nil); err != nil {
		t.Fatalf("clear alice appends: %v", err)
	}
	if got, err := GetChatPromptAppendIDs(aliceCtx, database, aliceChat); err != nil || len(got) != 0 {
		t.Fatalf("alice chat appends = %v, %v, want pruned", got, err)
	}
	if got, err := GetChatPromptAppendIDs(bobCtx, database, bobChat); err != nil || !slices.Equal(got, []string{"shared"}) {
		t.Fatalf("bob chat appends = %v, %v, want preserved", got, err)
	}
	if got, err := GetPromptAppends(bobCtx, database); err != nil || !slices.Equal(got, []PromptAppend{appendValue}) {
		t.Fatalf("bob appends = %#v, %v, want preserved shared append", got, err)
	}

	const serverID = "mcp-0123456789abcdef"
	capability := MCPServerCapability(serverID)
	aliceServer := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "Alice MCP", URL: "https://alice.example/mcp"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "alice-token",
	}}
	bobServer := []MCPServerUpdate{{
		Server:              MCPServer{ID: serverID, Name: "Bob MCP", URL: "https://bob.example/mcp"},
		AuthorizationChange: MCPReplaceAuthorization,
		AuthorizationValue:  "bob-token",
	}}
	if err := setMCPServers(aliceCtx, database, aliceServer); err != nil {
		t.Fatalf("set alice mcp: %v", err)
	}
	if err := setMCPServers(bobCtx, database, bobServer); err != nil {
		t.Fatalf("set bob mcp: %v", err)
	}
	if err := setDefaultEnabledTools(aliceCtx, database, []string{capability}); err != nil {
		t.Fatal(err)
	}
	if err := setDefaultEnabledTools(bobCtx, database, []string{capability}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertChat(aliceCtx, database, aliceChat, "", []string{capability}, nil); err != nil {
		t.Fatalf("set alice chat tools: %v", err)
	}
	if err := UpsertChat(bobCtx, database, bobChat, "", []string{capability}, nil); err != nil {
		t.Fatalf("set bob chat tools: %v", err)
	}

	if err := setMCPServers(aliceCtx, database, nil); err != nil {
		t.Fatalf("clear alice mcp: %v", err)
	}
	if got, err := GetDefaultEnabledTools(aliceCtx, database, nil); err != nil || len(got) != 0 {
		t.Fatalf("alice default tools = %v, %v, want pruned", got, err)
	}
	if got, err := GetChatTools(aliceCtx, database, aliceChat); err != nil || len(got) != 0 {
		t.Fatalf("alice chat tools = %v, %v, want pruned", got, err)
	}
	if got, err := GetDefaultEnabledTools(bobCtx, database, nil); err != nil || !slices.Equal(got, []string{capability}) {
		t.Fatalf("bob default tools = %v, %v, want preserved", got, err)
	}
	if got, err := GetChatTools(bobCtx, database, bobChat); err != nil || !slices.Equal(got, []string{capability}) {
		t.Fatalf("bob chat tools = %v, %v, want preserved", got, err)
	}
	bobConfigs, err := GetMCPServerConfigs(bobCtx, database, []string{capability})
	if err != nil || len(bobConfigs) != 1 || bobConfigs[0].AuthorizationToken != "bob-token" {
		t.Fatalf("bob mcp configs = %#v, %v", bobConfigs, err)
	}
	if got, err := GetMCPServers(aliceCtx, database); err != nil || len(got) != 0 {
		t.Fatalf("alice mcp servers = %#v, %v, want empty", got, err)
	}
}

func TestUserChatAndMessageReadsAreIsolated(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	if err := UpsertChat(aliceCtx, database, 101, "Alice chat", []string{"webfetch"}, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertChat(bobCtx, database, 102, "Bob chat", []string{"git"}, []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	aliceMessageID, err := InsertMessage(aliceCtx, database, 101, 0, llm.Message{Role: "user", Content: "alice question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InsertMessage(bobCtx, database, 102, 0, llm.Message{Role: "user", Content: "bob question"}); err != nil {
		t.Fatal(err)
	}

	owns, err := OwnsChat(aliceCtx, database, 101)
	if err != nil || !owns {
		t.Fatalf("alice owns 101 = %v, %v", owns, err)
	}
	if owns, err := OwnsChat(bobCtx, database, 101); err != nil || owns {
		t.Fatalf("bob owns alice chat = %v, %v, want false", owns, err)
	}
	chats, err := GetChatsPage(aliceCtx, database, "", 0, false, 100)
	if err != nil || len(chats) != 1 || chats[0].ID != 101 || chats[0].Title != "Alice chat" {
		t.Fatalf("alice chats = %#v, %v", chats, err)
	}
	if messages, err := GetMessages(aliceCtx, database, 101); err != nil || len(messages) != 1 || messages[0].Content != "alice question" {
		t.Fatalf("alice messages = %#v, %v", messages, err)
	}

	if _, err := GetMessages(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob GetMessages(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := GetMessageSnapshot(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob snapshot(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := GetChatTools(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob GetChatTools(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := GetChatPromptAppendIDs(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob GetChatPromptAppendIDs(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := InsertMessage(bobCtx, database, 101, 1, llm.Message{Role: "user", Content: "intrusion"}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob InsertMessage(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := ReplaceUserMessage(bobCtx, database, 101, aliceMessageID, llm.Message{Role: "user", Content: "edited"}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob ReplaceUserMessage(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := UndoLatestTurn(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob UndoLatestTurn(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if _, err := RedoLatestTurn(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob RedoLatestTurn(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := DiscardUndoneMessages(bobCtx, database, 101); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob DiscardUndoneMessages(alice chat) error = %v, want ErrChatNotFound", err)
	}
	aliceSnapshot, err := GetMessageSnapshot(aliceCtx, database, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendCompletion(bobCtx, database, 101, aliceSnapshot, []llm.Message{{Role: "assistant", Content: "bad"}}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob AppendCompletion(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := UpsertChat(bobCtx, database, 101, "Stolen", []string{"stolen"}, []string{"stolen"}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob UpsertChat(alice chat) error = %v, want ErrChatNotFound", err)
	}

	var title string
	if err := database.QueryRow(`SELECT title FROM chats WHERE id = 101`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Alice chat" {
		t.Fatalf("alice title after foreign upsert = %q, want preserved", title)
	}
	if got, err := GetChatTools(aliceCtx, database, 101); err != nil || !slices.Equal(got, []string{"webfetch"}) {
		t.Fatalf("alice tools after foreign upsert = %v, %v", got, err)
	}
	if messages, err := GetMessages(aliceCtx, database, 101); err != nil || len(messages) != 1 {
		t.Fatalf("alice messages after foreign writes = %#v, %v", messages, err)
	}
}

func TestChatPinsOrderAndPaginatePerUser(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	bob, bobCtx := createIsolationUser(t, database, "Bob")

	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title, updated_at, pinned) VALUES
			(1, ?, 'Pinned tie first', '2026-01-01T00:00:00.000Z', 1),
			(2, ?, 'Pinned newest', '2026-01-04T00:00:00.000Z', 1),
			(3, ?, 'Unpinned newest', '2026-01-05T00:00:00.000Z', 0),
			(4, ?, 'Unpinned oldest', '2026-01-02T00:00:00.000Z', 0),
			(5, ?, 'Pinned tie second', '2026-01-01T00:00:00.000Z', 1),
			(9, ?, 'Bob pinned', '2026-01-09T00:00:00.000Z', 1);
		INSERT INTO messages (chat_id, position, role, content) VALUES
			(1, 0, 'user', 'one'),
			(2, 0, 'user', 'two'),
			(3, 0, 'user', 'three'),
			(4, 0, 'user', 'four'),
			(5, 0, 'user', 'five'),
			(9, 0, 'user', 'nine');
		UPDATE chats SET updated_at = CASE id
			WHEN 1 THEN '2026-01-01T00:00:00.000Z'
			WHEN 2 THEN '2026-01-04T00:00:00.000Z'
			WHEN 3 THEN '2026-01-05T00:00:00.000Z'
			WHEN 4 THEN '2026-01-02T00:00:00.000Z'
			WHEN 5 THEN '2026-01-01T00:00:00.000Z'
			WHEN 9 THEN '2026-01-09T00:00:00.000Z'
		END
		WHERE id IN (1, 2, 3, 4, 5, 9);
	`, alice.ID, alice.ID, alice.ID, alice.ID, alice.ID, bob.ID); err != nil {
		t.Fatalf("insert pinned chats: %v", err)
	}

	wantPages := [][]int64{{2, 5}, {1, 3}, {4}}
	var cursorUpdatedAt string
	var cursorID int64
	var cursorPinned bool
	for pageIndex, want := range wantPages {
		chats, err := GetChatsPage(aliceCtx, database, cursorUpdatedAt, cursorID, cursorPinned, 2)
		if err != nil {
			t.Fatalf("get pinned page %d: %v", pageIndex, err)
		}
		got := make([]int64, len(chats))
		for index, chat := range chats {
			got[index] = chat.ID
		}
		if !slices.Equal(got, want) {
			t.Fatalf("pinned page %d IDs = %v, want %v", pageIndex, got, want)
		}
		last := chats[len(chats)-1]
		cursorUpdatedAt, cursorID, cursorPinned = last.UpdatedAt, last.ID, last.Pinned
	}

	bobChats, err := GetChatsPage(bobCtx, database, "", 0, false, 10)
	if err != nil {
		t.Fatalf("get bob chats: %v", err)
	}
	if len(bobChats) != 1 || bobChats[0].ID != 9 || !bobChats[0].Pinned {
		t.Fatalf("bob chats = %#v, want only pinned chat 9", bobChats)
	}

	var updatedAt string
	if err := database.QueryRow(`SELECT updated_at FROM chats WHERE id = 1`).Scan(&updatedAt); err != nil {
		t.Fatalf("read alice update time: %v", err)
	}
	if err := SetChatPinned(bobCtx, database, 1, false); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("bob unpin(alice chat) error = %v, want ErrChatNotFound", err)
	}
	if err := SetChatPinned(aliceCtx, database, 1, false); err != nil {
		t.Fatalf("alice unpin(chat 1) error: %v", err)
	}
	var pinned int
	var storedUpdatedAt string
	if err := database.QueryRow(`SELECT pinned, updated_at FROM chats WHERE id = 1`).Scan(&pinned, &storedUpdatedAt); err != nil {
		t.Fatalf("read alice pin state: %v", err)
	}
	if pinned != 0 || storedUpdatedAt != updatedAt {
		t.Fatalf("alice chat 1 pin = %d, updated_at = %q; want 0 and %q", pinned, storedUpdatedAt, updatedAt)
	}
	if err := SetChatPinned(aliceCtx, database, 999, true); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("alice pin(missing chat) error = %v, want ErrChatNotFound", err)
	}
}

func TestLoadChatToolsRechecksChatOwnership(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")

	if err := UpsertChat(aliceCtx, database, 301, "Alice chat", []string{"webfetch", "git"}, nil); err != nil {
		t.Fatalf("create alice chat: %v", err)
	}
	if err := UpsertChat(bobCtx, database, 302, "Bob chat", []string{"websearch"}, nil); err != nil {
		t.Fatalf("create bob chat: %v", err)
	}

	bobChats := []Chat{{ID: 301, Tools: []string{}}, {ID: 302, Tools: []string{}}}
	if err := loadChatTools(bobCtx, database, bobChats); err != nil {
		t.Fatalf("load bob chat tools: %v", err)
	}
	if len(bobChats[0].Tools) != 0 {
		t.Fatalf("bob loaded foreign tools = %v, want none", bobChats[0].Tools)
	}
	if !slices.Equal(bobChats[1].Tools, []string{"websearch"}) {
		t.Fatalf("bob own tools = %v, want [websearch]", bobChats[1].Tools)
	}

	aliceChats := []Chat{{ID: 301, Tools: []string{}}, {ID: 302, Tools: []string{}}}
	if err := loadChatTools(aliceCtx, database, aliceChats); err != nil {
		t.Fatalf("load alice chat tools: %v", err)
	}
	if !slices.Equal(aliceChats[0].Tools, []string{"webfetch", "git"}) {
		t.Fatalf("alice own tools = %v, want [webfetch git]", aliceChats[0].Tools)
	}
	if len(aliceChats[1].Tools) != 0 {
		t.Fatalf("alice loaded foreign tools = %v, want none", aliceChats[1].Tools)
	}
}

func TestAllocateChatCleanupStaysPerUser(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	bob, bobCtx := createIsolationUser(t, database, "Bob")

	if _, err := database.Exec(`
		INSERT INTO chats (id, user_id, title, created_at, updated_at)
		VALUES (201, ?, 'alice abandoned', strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-48 hours'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-48 hours')),
		       (202, ?, 'bob abandoned', strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-48 hours'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-48 hours'))
	`, alice.ID, bob.ID); err != nil {
		t.Fatal(err)
	}

	newChat, err := AllocateChat(aliceCtx, database, nil, nil)
	if err != nil {
		t.Fatalf("allocate alice chat: %v", err)
	}
	var aliceOld, bobOld, ownerID int64
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats WHERE id = 201`).Scan(&aliceOld); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM chats WHERE id = 202`).Scan(&bobOld); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT user_id FROM chats WHERE id = ?`, newChat).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if aliceOld != 0 || bobOld != 1 || ownerID != alice.ID {
		t.Fatalf("abandoned alice=%d bob=%d new owner=%d, want 0,1,%d", aliceOld, bobOld, ownerID, alice.ID)
	}
	if chats, err := GetChatsPage(bobCtx, database, "", 0, false, 100); err != nil || len(chats) != 0 {
		t.Fatalf("bob chats after alice cleanup = %#v, %v", chats, err)
	}
}

func TestModelEndpointPreferencesArePerUser(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	alice, aliceCtx := createIsolationUser(t, database, "Alice")
	_, bobCtx := createIsolationUser(t, database, "Bob")
	_, carolCtx := createIsolationUser(t, database, "Carol")

	if err := SetModelEndpointType(aliceCtx, database, "shared-model", llm.EndpointResponses); err != nil {
		t.Fatal(err)
	}
	if err := SetModelEndpointType(bobCtx, database, "shared-model", llm.EndpointChatCompletions); err != nil {
		t.Fatal(err)
	}
	if got, err := GetModelEndpointType(aliceCtx, database, "shared-model"); err != nil || got != llm.EndpointResponses {
		t.Fatalf("alice endpoint type = %q, %v", got, err)
	}
	if got, err := GetModelEndpointType(bobCtx, database, "shared-model"); err != nil || got != llm.EndpointChatCompletions {
		t.Fatalf("bob endpoint type = %q, %v", got, err)
	}
	if got, err := GetModelEndpointType(carolCtx, database, "shared-model"); err != nil || got != "" {
		t.Fatalf("carol endpoint type = %q, %v, want empty", got, err)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM model_endpoint_preferences WHERE model = 'shared-model'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("shared model preference rows = %d, want 2", rows)
	}
	var ownerRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM model_endpoint_preferences WHERE user_id = ?`, alice.ID).Scan(&ownerRows); err != nil {
		t.Fatal(err)
	}
	if ownerRows != 1 {
		t.Fatalf("alice preference rows = %d, want 1", ownerRows)
	}
}

func TestLegacyOwnerClaimAndSessionLifecycle(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	if _, err := GetUserByUsername(ctx, database, "skrittle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unclaimed Skrittle lookup error = %v, want sql.ErrNoRows", err)
	}
	var seedUsername, seedHash string
	var seedDisabled int
	if err := database.QueryRow(`
		SELECT username, password_hash, disabled FROM users WHERE id = ?
	`, LegacyOwnerID).Scan(&seedUsername, &seedHash, &seedDisabled); err != nil {
		t.Fatalf("load seeded legacy owner: %v", err)
	}
	if seedUsername != LegacyOwnerUsername || seedHash != "" || seedDisabled != 1 {
		t.Fatalf("seeded legacy owner = %q hash %q disabled %d", seedUsername, seedHash, seedDisabled)
	}

	ownerHash := "encoded-owner"
	owner, err := CreateUser(ctx, database, "SKRITTLE", ownerHash)
	if err != nil {
		t.Fatalf("claim legacy owner: %v", err)
	}
	if owner.ID != LegacyOwnerID || owner.Username != LegacyOwnerUsername || owner.PasswordHash != ownerHash {
		t.Fatalf("owner after claim = %#v", owner)
	}
	for _, username := range []string{"Skrittle", "skrittle", "SKRITTLE"} {
		if _, err := CreateUser(ctx, database, username, "replacement-hash"); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("CreateUser(%q) error = %v, want ErrUsernameTaken", username, err)
		}
	}
	owner, err = GetUserByUsername(ctx, database, "skrittle")
	if err != nil || owner.ID != LegacyOwnerID || owner.Username != LegacyOwnerUsername || owner.PasswordHash != ownerHash {
		t.Fatalf("owner after repeated signup = %#v, %v", owner, err)
	}

	alice, err := CreateUser(ctx, database, "Alice", "encoded-alice")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := CreateUser(ctx, database, "alice", "other-hash"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username error = %v, want ErrUsernameTaken", err)
	}
	byCase, err := GetUserByUsername(ctx, database, "ALICE")
	if err != nil || byCase.ID != alice.ID || byCase.Username != "Alice" {
		t.Fatalf("case-insensitive lookup = %#v, %v", byCase, err)
	}
	if _, err := GetUserByUsername(ctx, database, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing user error = %v, want sql.ErrNoRows", err)
	}

	now := time.Now()
	if err := CreateSession(ctx, database, alice.ID, "token-one", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := CreateSession(ctx, database, alice.ID, "token-two", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sessionUser, err := GetSessionUser(ctx, database, "token-one", now)
	if err != nil || sessionUser.ID != alice.ID {
		t.Fatalf("session one user = %#v, %v", sessionUser, err)
	}
	if err := DeleteSession(ctx, database, "token-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetSessionUser(ctx, database, "token-one", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session error = %v, want sql.ErrNoRows", err)
	}
	if sessionUser, err := GetSessionUser(ctx, database, "token-two", now); err != nil || sessionUser.ID != alice.ID {
		t.Fatalf("second session survived logout = %#v, %v", sessionUser, err)
	}
	if err := DeleteSession(ctx, database, "unknown"); err != nil {
		t.Fatalf("delete unknown session: %v", err)
	}

	if err := CreateSession(ctx, database, alice.ID, "expired-token", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := GetSessionUser(ctx, database, "expired-token", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired session error = %v, want sql.ErrNoRows", err)
	}
	if _, err := database.Exec(`
		INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ('stale-token', ?, ?)
	`, alice.ID, now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := CreateSession(ctx, database, alice.ID, "fresh-token", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stale int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = 'stale-token'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("stale expired session rows = %d, want cleaned", stale)
	}

	disabled, err := CreateUser(ctx, database, "Disabled", "encoded-disabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	if err := CreateSession(ctx, database, disabled.ID, "disabled-token", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := GetSessionUser(ctx, database, "disabled-token", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled user session error = %v, want sql.ErrNoRows", err)
	}
	if _, err := GetUserByUsername(ctx, database, "Disabled"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled username lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestLegacyAccountClaimPreservesInitializedCredentials(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	alice, err := CreateUser(ctx, database, "Alice", "alice-hash")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if alice.ID == LegacyOwnerID {
		t.Fatal("ordinary signup before claim received the legacy owner ID")
	}
	if _, err := CreateUser(ctx, database, "Alice", "other-hash"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate Alice error = %v, want ErrUsernameTaken", err)
	}

	owner, err := CreateUser(ctx, database, "Skrittle", "first-hash")
	if err != nil {
		t.Fatalf("claim legacy owner: %v", err)
	}
	if owner.PasswordHash != "first-hash" {
		t.Fatalf("claimed owner hash = %q, want first-hash", owner.PasswordHash)
	}
	if _, err := CreateUser(ctx, database, "skrittle", "second-hash"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("owner takeover error = %v, want ErrUsernameTaken", err)
	}
	owner, err = GetUserByUsername(ctx, database, "SKRITTLE")
	if err != nil || owner.PasswordHash != "first-hash" {
		t.Fatalf("owner credentials changed: %#v, %v", owner, err)
	}
}

func TestUsernamePolicyAndInvalidUserContext(t *testing.T) {
	valid := []string{"abc", "Skrittle", "a_b.c-d", strings.Repeat("a", 32)}
	for _, username := range valid {
		if err := ValidateUsername(username); err != nil {
			t.Errorf("ValidateUsername(%q) error = %v", username, err)
		}
	}
	invalid := []string{"", "ab", strings.Repeat("a", 33), " Alice", "Alice ", "al ice", ".alice", "-alice", "ali@ce", "álice"}
	for _, username := range invalid {
		if err := ValidateUsername(username); err == nil {
			t.Errorf("ValidateUsername(%q) error = nil", username)
		}
	}

	database := openMessagesTestDatabase(t, "")
	invalidCtx := WithUser(context.Background(), 0)
	if got := UserID(invalidCtx); got != 0 {
		t.Fatalf("explicit zero user ID = %d, want 0", got)
	}
	if got := UserID(context.Background()); got != LegacyOwnerID {
		t.Fatalf("absent user ID = %d, want legacy owner %d", got, LegacyOwnerID)
	}
	if _, err := GetTheme(invalidCtx, database); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("GetTheme(invalid context) error = %v, want ErrInvalidUser", err)
	}
	if _, err := GetChatsPage(invalidCtx, database, "", 0, false, 10); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("GetChatsPage(invalid context) error = %v, want ErrInvalidUser", err)
	}
	if _, err := OwnsChat(invalidCtx, database, 1); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("OwnsChat(invalid context) error = %v, want ErrInvalidUser", err)
	}
	if err := SaveSettings(invalidCtx, database, SettingsUpdate{Model: "model", MaxToolRounds: 1}); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("SaveSettings(invalid context) error = %v, want ErrInvalidUser", err)
	}
	if err := CreateSession(context.Background(), database, 0, "token", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("CreateSession(user 0) error = nil")
	}
	if _, err := CreateUser(context.Background(), database, "EmptyHash", ""); err == nil {
		t.Fatal("CreateUser(empty hash) error = nil")
	}
	if _, err := CreateUser(context.Background(), database, "Skrittle", ""); err == nil {
		t.Fatal("CreateUser(Skrittle, empty hash) error = nil")
	}
}

func TestNewUserStartsWithBuiltInDefaults(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ownerCtx := WithUser(context.Background(), LegacyOwnerID)
	if err := SaveSettings(ownerCtx, database, SettingsUpdate{
		Model:         "owner-model",
		MaxToolRounds: 9,
		DefaultTools:  []string{"webfetch"},
		PromptAppends: []PromptAppend{{ID: "owner", Name: "Owner", Text: "Owner only."}},
		Ntfy: &NtfySettingsUpdate{
			Endpoint: "https://owner.example", Topic: "owner-topic",
			APIKeyChange: NtfyReplaceAPIKey, APIKeyValue: "owner-secret",
		},
		LLM: &LLMSettingsUpdate{
			Endpoint: "https://owner.example/v1/responses", APIKeyChange: LLMReplaceAPIKey, APIKeyValue: "owner-llm",
		},
		Theme: "forest-night",
	}); err != nil {
		t.Fatal(err)
	}

	_, newCtx := createIsolationUser(t, database, "NewUser")
	if got, err := GetDefaultModel(newCtx, database, "fallback"); err != nil || got != "fallback" {
		t.Fatalf("new user default model = %q, %v, want fallback", got, err)
	}
	if got, err := GetMaxToolRounds(newCtx, database, 16); err != nil || got != 16 {
		t.Fatalf("new user max rounds = %d, %v, want fallback", got, err)
	}
	if got, err := GetDefaultEnabledTools(newCtx, database, []string{"git"}); err != nil || !slices.Equal(got, []string{"git"}) {
		t.Fatalf("new user default tools = %v, %v, want fallback", got, err)
	}
	if got, err := GetPromptAppends(newCtx, database); err != nil || !slices.Equal(got, DefaultPromptAppends()) {
		t.Fatalf("new user appends = %#v, %v, want built-ins", got, err)
	}
	if got, err := GetTheme(newCtx, database); err != nil || got != "" {
		t.Fatalf("new user theme = %q, %v, want empty", got, err)
	}
	if ntfy, err := GetNtfySettings(newCtx, database); err != nil || ntfy.APIKeyConfigured || ntfy.Endpoint != "" {
		t.Fatalf("new user ntfy = %#v, %v, want empty", ntfy, err)
	}
	if llmConfig, err := GetLLMConfig(newCtx, database); err != nil || llmConfig.APIKey != "" || llmConfig.Endpoint != "" {
		t.Fatalf("new user llm = %#v, %v, want empty", llmConfig, err)
	}
	if servers, err := GetMCPServers(newCtx, database); err != nil || len(servers) != 0 {
		t.Fatalf("new user mcp servers = %#v, %v, want empty", servers, err)
	}
}

func TestTitleGenerationSettingsAreIsolated(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	_, aliceCtx := createIsolationUser(t, database, "TitleAlice")
	_, bobCtx := createIsolationUser(t, database, "TitleBob")

	if err := SaveSettings(aliceCtx, database, SettingsUpdate{
		Model:         "alice-model",
		MaxToolRounds: 8,
		TitleGeneration: &TitleGenerationSettingsUpdate{
			Enabled: true,
			Model:   "alice-title-model",
		},
	}); err != nil {
		t.Fatalf("save alice title settings: %v", err)
	}
	aliceSettings, err := GetTitleGenerationSettings(aliceCtx, database)
	if err != nil || !aliceSettings.Enabled || aliceSettings.Model != "alice-title-model" {
		t.Fatalf("alice title settings = %#v, %v", aliceSettings, err)
	}
	bobSettings, err := GetTitleGenerationSettings(bobCtx, database)
	if err != nil || bobSettings.Enabled || bobSettings.Model != "" {
		t.Fatalf("bob title settings = %#v, %v, want defaults", bobSettings, err)
	}
}
