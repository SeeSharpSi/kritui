package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"seesharpsi/kritui/commands"
	kritui_db "seesharpsi/kritui/db"
	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/ntfy"
	"seesharpsi/kritui/templ"
	"seesharpsi/kritui/themes"
	"seesharpsi/kritui/tools"
)

const (
	defaultHistoryPageSize = 10
	maxHistoryPageSize     = 50
)

func homeHandler(database *sql.DB, toolRegistry *tools.Registry, commandRegistry *commands.Registry, toolCalls *toolCallStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		chatURL := ""
		var promptAppends []kritui_db.PromptAppend
		var defaultTools []string
		defaultToolsLoaded := false
		if chat == "" {
			var err error
			defaultTools, err = kritui_db.GetDefaultEnabledTools(r.Context(), database, nil)
			if err != nil {
				log.Printf("get default tools: %v", err)
				renderPageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
				return
			}
			defaultToolsLoaded = true
			promptAppends, err = kritui_db.GetPromptAppends(r.Context(), database)
			if err != nil {
				log.Printf("get prompt appends: %v", err)
				renderPageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
				return
			}
			chatID, err := kritui_db.AllocateChat(r.Context(), database, defaultTools, kritui_db.DefaultPromptAppendIDs(promptAppends))
			if err != nil {
				log.Printf("allocate chat: %v", err)
				renderPageError(w, r, http.StatusInternalServerError, "Failed to allocate chat.")
				return
			}

			chat = strconv.FormatInt(chatID, 10)
			query := r.URL.Query()
			query.Set("chat", chat)
			target := *r.URL
			target.RawQuery = query.Encode()
			chatURL = target.String()
		}

		chatID, ok := positiveID(chat)
		if !ok {
			renderPageError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		messages, err := kritui_db.GetMessages(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get messages: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load messages.")
			return
		}
		completion, completionActive := toolCalls.active(chatID)
		if !completionActive && len(messages) > 0 && messages[len(messages)-1].Role == "user" {
			// Completion persistence happens before the active marker is released.
			// Re-read once to close the small race between those two observations.
			messages, err = kritui_db.GetMessages(r.Context(), database, chatID)
			if err != nil {
				log.Printf("reload messages after completion: %v", err)
				renderPageError(w, r, http.StatusInternalServerError, "Failed to load messages.")
				return
			}
		}
		if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
			completion = activeCompletion{}
			completionActive = false
		}
		enabledTools, err := kritui_db.GetChatTools(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get chat tools: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load chat tools.")
			return
		}
		enabledAppendIDs, err := kritui_db.GetChatPromptAppendIDs(r.Context(), database, chatID)
		if err != nil {
			log.Printf("get chat prompt append IDs: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load chat appends.")
			return
		}
		renderSettings, err := loadHomeRenderSettings(r.Context(), database, promptAppends, defaultTools, defaultToolsLoaded)
		if err != nil {
			log.Printf("load home render settings: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		selectedModel := renderSettings.defaultModel
		for index := len(messages) - 1; index >= 0; index-- {
			if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Model) != "" {
				selectedModel = messages[index].Model
				break
			}
		}
		if completionActive {
			if completion.model != "" {
				selectedModel = completion.model
			}
			enabledTools = completion.tools
		}
		models, selectedModel := availableModels(r, selectedModel)
		if renderSettings.defaultModel != "" && !slices.Contains(models, renderSettings.defaultModel) {
			models = append([]string{renderSettings.defaultModel}, models...)
		}

		var page bytes.Buffer
		home := templates.HomeData{
			ChatID:              chat,
			ChatURL:             chatURL,
			Messages:            messages,
			Models:              models,
			SelectedModel:       selectedModel,
			DefaultModel:        renderSettings.defaultModel,
			MaxToolRounds:       renderSettings.maxToolRounds,
			Tools:               toolRegistry.Names(),
			EnabledTools:        enabledTools,
			DefaultTools:        renderSettings.defaultTools,
			MCPServers:          renderSettings.mcpServers,
			PromptAppends:       renderSettings.promptAppends,
			NtfySettings:        renderSettings.ntfySettings,
			EnabledAppendIDs:    enabledAppendIDs,
			CompletionRequestID: completion.requestID,
			CompletionStarted:   completion.started,
			CommandDefinitions:  commandRegistry.Definitions(),
			Theme:               renderSettings.theme,
		}
		if err := templates.Home(home).Render(r.Context(), &page); err != nil {
			log.Printf("render page: %v", err)
			renderPageError(w, r, http.StatusInternalServerError, "Failed to render page.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page.Bytes())
	}
}

// homeRenderSettings gathers the configuration values the chat page renders.
// Prompt appends and default tools may arrive preloaded from chat allocation.
type homeRenderSettings struct {
	promptAppends []kritui_db.PromptAppend
	defaultModel  string
	maxToolRounds int
	defaultTools  []string
	mcpServers    []kritui_db.MCPServer
	ntfySettings  kritui_db.NtfySettings
	theme         themes.Theme
}

// loadHomeRenderSettings fetches every not-yet-loaded settings value in the
// page's historical query order. All failures collapse to one error because
// the handler renders the same message for each of them; specific causes stay
// in the log.
func loadHomeRenderSettings(ctx context.Context, database *sql.DB, promptAppends []kritui_db.PromptAppend, defaultTools []string, defaultToolsLoaded bool) (homeRenderSettings, error) {
	settings := homeRenderSettings{
		promptAppends: promptAppends,
		defaultTools:  defaultTools,
	}
	var err error
	if settings.promptAppends == nil {
		if settings.promptAppends, err = kritui_db.GetPromptAppends(ctx, database); err != nil {
			log.Printf("get prompt appends: %v", err)
			return homeRenderSettings{}, fmt.Errorf("load prompt appends: %w", err)
		}
	}
	if settings.defaultModel, err = kritui_db.GetDefaultModel(ctx, database, os.Getenv("LLM_MODEL")); err != nil {
		log.Printf("get default model: %v", err)
		return homeRenderSettings{}, fmt.Errorf("load default model: %w", err)
	}
	if settings.maxToolRounds, err = kritui_db.GetMaxToolRounds(ctx, database, llm.DefaultMaxToolCallRounds); err != nil {
		log.Printf("get max tool rounds: %v", err)
		return homeRenderSettings{}, fmt.Errorf("load max tool rounds: %w", err)
	}
	if !defaultToolsLoaded {
		if settings.defaultTools, err = kritui_db.GetDefaultEnabledTools(ctx, database, nil); err != nil {
			log.Printf("get default tools: %v", err)
			return homeRenderSettings{}, fmt.Errorf("load default tools: %w", err)
		}
	}
	if settings.mcpServers, err = kritui_db.GetMCPServers(ctx, database); err != nil {
		log.Printf("get MCP servers: %v", err)
		return homeRenderSettings{}, fmt.Errorf("load MCP servers: %w", err)
	}
	if settings.ntfySettings, err = kritui_db.GetNtfySettings(ctx, database); err != nil {
		log.Printf("get ntfy settings: %v", err)
		return homeRenderSettings{}, fmt.Errorf("load ntfy settings: %w", err)
	}
	if settings.theme, err = loadStoredTheme(ctx, database); err != nil {
		log.Printf("load theme: %v", err)
		return homeRenderSettings{}, fmt.Errorf("load theme: %w", err)
	}
	return settings, nil
}

// loadStoredTheme resolves the persisted theme slug to a built-in theme,
// falling back to the default when unset or unrecognized.
func loadStoredTheme(ctx context.Context, database *sql.DB) (themes.Theme, error) {
	stored, err := kritui_db.GetTheme(ctx, database)
	if err != nil {
		return themes.Theme{}, fmt.Errorf("get stored theme: %w", err)
	}
	theme, err := themes.ByID(stored)
	if err != nil {
		theme = themes.Default()
	}
	return theme, nil
}

func historyHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat := r.URL.Query().Get("chat")
		if _, ok := positiveID(chat); !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		beforeUpdatedAt := r.URL.Query().Get("before")
		beforeID, ok := historyCursorID(r, beforeUpdatedAt)
		if !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid history cursor is required.")
			return
		}
		limit, ok := historyPageLimit(r)
		if !ok {
			renderHistoryLoadError(w, r, http.StatusBadRequest, "A valid history limit is required.")
			return
		}
		if err := renderHistoryEntries(r.Context(), w, database, chat, beforeUpdatedAt, beforeID, limit); err != nil {
			log.Printf("render chat history: %v", err)
			renderHistoryLoadError(w, r, http.StatusInternalServerError, "Failed to load chat history.")
		}
	}
}

func settingsHandler(database *sql.DB, registry *tools.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := templates.SettingsPanelData{
			ChatID:        r.URL.Query().Get("chat"),
			SelectedModel: os.Getenv("LLM_MODEL"),
			MaxToolRounds: llm.DefaultMaxToolCallRounds,
			ToolNames:     registry.Names(),
			PromptAppends: kritui_db.DefaultPromptAppends(),
			Themes:        themes.Options(),
		}
		render := func(status int, message string) {
			page.ErrorMessage = message
			renderSettingsPage(w, r, status, page)
		}
		if _, ok := positiveID(page.ChatID); !ok {
			render(http.StatusBadRequest, "A valid chat is required.")
			return
		}

		selectedModel, err := kritui_db.GetDefaultModel(r.Context(), database, page.SelectedModel)
		if err != nil {
			log.Printf("get default model: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.SelectedModel = selectedModel
		maxToolRounds, err := kritui_db.GetMaxToolRounds(r.Context(), database, page.MaxToolRounds)
		if err != nil {
			log.Printf("get max tool rounds: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.MaxToolRounds = maxToolRounds
		defaultTools, err := kritui_db.GetDefaultEnabledTools(r.Context(), database, nil)
		if err != nil {
			log.Printf("get default tools: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.DefaultTools = defaultTools
		promptAppends, err := kritui_db.GetPromptAppends(r.Context(), database)
		if err != nil {
			log.Printf("get prompt appends: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.PromptAppends = promptAppends
		mcpServers, err := kritui_db.GetMCPServers(r.Context(), database)
		if err != nil {
			log.Printf("get MCP servers: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.MCPServers = mcpServers
		ntfySettings, err := kritui_db.GetNtfySettings(r.Context(), database)
		if err != nil {
			log.Printf("get ntfy settings: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.NtfySettings = ntfySettings
		theme, err := loadStoredTheme(r.Context(), database)
		if err != nil {
			log.Printf("load theme: %v", err)
			render(http.StatusInternalServerError, "Failed to load settings.")
			return
		}
		page.SelectedTheme = theme
		if r.Method == http.MethodPost {
			if err := parseLimitedForm(w, r, maxSettingsBodyBytes); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					render(http.StatusRequestEntityTooLarge, "Request body is too large.")
					return
				}
				render(http.StatusBadRequest, "Invalid settings form.")
				return
			}

			submittedModel := strings.TrimSpace(r.FormValue("model"))
			submittedRounds, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("max_tool_rounds")))
			submittedTheme := strings.TrimSpace(r.FormValue("theme"))
			actionTheme := page.SelectedTheme
			themeUnknown := false
			if submittedTheme != "" {
				if theme, themeErr := themes.ByID(submittedTheme); themeErr == nil {
					actionTheme = theme
				} else {
					themeUnknown = true
				}
			}

			ntfySubmitted := r.FormValue("ntfy_form") == "1"
			ntfyAPIKey := ""
			clearNtfyAPIKey := false
			if ntfySubmitted {
				page.NtfySettings.Endpoint = strings.TrimSpace(r.FormValue("ntfy_endpoint"))
				page.NtfySettings.Topic = strings.TrimSpace(r.FormValue("ntfy_topic"))
				ntfyAPIKey = strings.TrimSpace(r.FormValue("ntfy_api_key"))
				clearNtfyAPIKey = r.FormValue("clear_ntfy_api_key") == "1"
				page.ClearNtfyAPIKey = clearNtfyAPIKey
			}

			submittedAppends := page.PromptAppends
			if r.FormValue("append_form") == "1" {
				submittedAppends, err = promptAppendsFromForm(r)
				if err != nil {
					page.SelectedModel = submittedModel
					page.PromptAppends = submittedAppends
					render(http.StatusBadRequest, fmt.Sprintf("Prompt append form is invalid: %v.", err))
					return
				}
			}

			mcpSubmitted := r.FormValue("mcp_form") == "1"
			submittedMCPServers := page.MCPServers
			var submittedMCPUpdates []kritui_db.MCPServerUpdate
			if mcpSubmitted {
				submittedMCPUpdates, err = mcpServersFromForm(r, page.MCPServers)
				submittedMCPServers = mcpServersFromUpdates(submittedMCPUpdates)
				page.MCPServers = submittedMCPServers
				page.MCPServersOpen = true
				page.ClearMCPAuthorizationIDs = mcpAuthorizationClearIDs(submittedMCPUpdates)
				if err != nil {
					page.SelectedModel = submittedModel
					render(http.StatusBadRequest, fmt.Sprintf("MCP server form is invalid: %v.", err))
					return
				}
			}

			submittedTools, submittedToolsErr := selectedDefaultTools(registry, submittedMCPServers, r.Form["default_tool"])
			actionModel, actionRounds, actionTools := page.SelectedModel, page.MaxToolRounds, page.DefaultTools
			if submittedModel != "" {
				actionModel = submittedModel
			}
			if submittedRounds >= 1 && submittedRounds <= kritui_db.MaxConfigurableToolRounds {
				actionRounds = submittedRounds
			}
			if submittedToolsErr == nil {
				actionTools = submittedTools
			}
			actionPage := func() templates.SettingsPanelData {
				value := page
				value.SelectedModel = actionModel
				value.MaxToolRounds = actionRounds
				value.DefaultTools = actionTools
				value.SelectedTheme = actionTheme
				value.PromptAppends = submittedAppends
				value.MCPServers = submittedMCPServers
				return value
			}

			if r.FormValue("mcp_action") == "add" {
				id, err := newMCPServerID()
				if err != nil {
					log.Printf("create MCP server ID: %v", err)
					value := actionPage()
					value.ErrorMessage = "Failed to add MCP server."
					value.MCPServersOpen = true
					renderSettingsPage(w, r, http.StatusInternalServerError, value)
					return
				}
				renderMCPServerEditor(w, r, http.StatusOK, kritui_db.MCPServer{ID: id, Name: "new server"})
				return
			}
			if removeID := strings.TrimSpace(r.FormValue("remove_mcp_server")); removeID != "" {
				if err := kritui_db.ValidateMCPServerID(removeID); err != nil {
					render(http.StatusBadRequest, fmt.Sprintf("MCP server remove is invalid: %v.", err))
					return
				}
				filtered := submittedMCPServers[:0]
				for _, server := range submittedMCPServers {
					if server.ID != removeID {
						filtered = append(filtered, server)
					}
				}
				removedCapability := kritui_db.MCPServerCapability(removeID)
				actionTools = slices.DeleteFunc(actionTools, func(name string) bool { return name == removedCapability })
				value := actionPage()
				value.DefaultTools = actionTools
				value.MCPServers = filtered
				value.MCPServersOpen = true
				renderSettingsPage(w, r, http.StatusOK, value)
				return
			}

			if r.FormValue("append_action") == "add" {
				id, err := newPromptAppendID()
				if err != nil {
					log.Printf("create prompt append ID: %v", err)
					value := actionPage()
					value.ErrorMessage = "Failed to add prompt append."
					renderSettingsPage(w, r, http.StatusInternalServerError, value)
					return
				}
				submittedAppends = append(submittedAppends, kritui_db.PromptAppend{ID: id, Name: "new append"})
				value := actionPage()
				value.PromptAppends = submittedAppends
				renderSettingsPage(w, r, http.StatusOK, value)
				return
			}
			if removeID := strings.TrimSpace(r.FormValue("remove_append")); removeID != "" {
				if err := kritui_db.ValidatePromptAppendID(removeID); err != nil {
					render(http.StatusBadRequest, fmt.Sprintf("Prompt append remove is invalid: %v.", err))
					return
				}
				filtered := submittedAppends[:0]
				for _, value := range submittedAppends {
					if value.ID != removeID {
						filtered = append(filtered, value)
					}
				}
				value := actionPage()
				value.PromptAppends = filtered
				renderSettingsPage(w, r, http.StatusOK, value)
				return
			}

			page.SelectedModel = submittedModel
			page.SelectedTheme = actionTheme
			page.MaxToolRounds = submittedRounds
			page.DefaultTools = submittedTools
			page.PromptAppends = submittedAppends
			page.MCPServers = submittedMCPServers
			if page.SelectedModel == "" {
				render(http.StatusBadRequest, "A model is required.")
				return
			}
			if submittedRounds < 1 || submittedRounds > kritui_db.MaxConfigurableToolRounds {
				render(http.StatusBadRequest, fmt.Sprintf("Max tool-call rounds must be between 1 and %d.", kritui_db.MaxConfigurableToolRounds))
				return
			}
			if themeUnknown {
				render(http.StatusBadRequest, "Theme selection is invalid.")
				return
			}
			if submittedToolsErr != nil {
				render(http.StatusBadRequest, "Tool selection is invalid.")
				return
			}
			if r.FormValue("append_form") == "1" {
				if err := kritui_db.ValidatePromptAppends(submittedAppends); err != nil {
					render(http.StatusBadRequest, fmt.Sprintf("Prompt append settings are invalid: %v.", err))
					return
				}
			}
			if mcpSubmitted {
				if err := kritui_db.ValidateMCPServers(submittedMCPUpdates); err != nil {
					render(http.StatusBadRequest, fmt.Sprintf("MCP server settings are invalid: %v.", err))
					return
				}
			}

			var ntfyUpdate *kritui_db.NtfySettingsUpdate
			if ntfySubmitted {
				endpoint := page.NtfySettings.Endpoint
				topic := page.NtfySettings.Topic
				if (endpoint == "") != (topic == "") {
					render(http.StatusBadRequest, "Endpoint and topic must both be set or empty.")
					return
				}
				if endpoint == "" && ntfyAPIKey != "" {
					render(http.StatusBadRequest, "Endpoint and topic are required when setting an API key.")
					return
				}
				if clearNtfyAPIKey && ntfyAPIKey != "" {
					render(http.StatusBadRequest, "Choose replacing or clearing API key, not both.")
					return
				}
				if endpoint != "" {
					if err := (ntfy.Config{Endpoint: endpoint, Topic: topic}).Validate(); err != nil {
						render(http.StatusBadRequest, fmt.Sprintf("Notification settings are invalid: %v.", err))
						return
					}
				}
				value := kritui_db.NtfySettingsUpdate{Endpoint: endpoint, Topic: topic}
				switch {
				case clearNtfyAPIKey:
					value.APIKeyChange = kritui_db.NtfyClearAPIKey
					page.NtfySettings.APIKeyConfigured = false
				case ntfyAPIKey != "":
					value.APIKeyChange = kritui_db.NtfyReplaceAPIKey
					value.APIKeyValue = ntfyAPIKey
					page.NtfySettings.APIKeyConfigured = true
				case endpoint == "":
					page.NtfySettings.APIKeyConfigured = false
				}
				ntfyUpdate = &value
			}

			update := kritui_db.SettingsUpdate{
				Model:         page.SelectedModel,
				MaxToolRounds: page.MaxToolRounds,
				DefaultTools:  page.DefaultTools,
				Ntfy:          ntfyUpdate,
				Theme:         submittedTheme,
			}
			if r.FormValue("append_form") == "1" {
				update.PromptAppends = submittedAppends
			}
			if mcpSubmitted {
				update.MCPServers = submittedMCPUpdates
			}
			if err := kritui_db.SaveSettings(r.Context(), database, update); err != nil {
				log.Printf("save settings: %v", err)
				render(http.StatusInternalServerError, "Failed to save settings.")
				return
			}
			if mcpSubmitted {
				page.MCPServers = mcpServersAfterSave(submittedMCPUpdates)
				page.ClearMCPAuthorizationIDs = nil
			}
			page.Saved = true
		}

		if page.Saved && r.Header.Get("HX-Request") == "true" {
			if r.FormValue("tool_selection") == "1" {
				page.EnabledToolNames, _ = selectedDefaultTools(registry, page.MCPServers, r.Form["tool"])
			} else {
				chatID, _ := positiveID(page.ChatID)
				page.EnabledToolNames, err = kritui_db.GetChatTools(r.Context(), database, chatID)
				if err != nil {
					log.Printf("get chat tools after settings save: %v", err)
					failurePage := page
					failurePage.Saved = false
					failurePage.ErrorMessage = "Failed to load chat tools."
					renderSettingsPage(w, r, http.StatusInternalServerError, failurePage)
					return
				}
			}
			if r.FormValue("append_selection") == "1" {
				available := make(map[string]struct{}, len(page.PromptAppends))
				for _, value := range page.PromptAppends {
					available[value.ID] = struct{}{}
				}
				seen := make(map[string]struct{}, len(r.Form["append"]))
				for _, id := range r.Form["append"] {
					if _, ok := available[id]; !ok {
						continue
					}
					if _, ok := seen[id]; ok {
						continue
					}
					seen[id] = struct{}{}
					page.EnabledAppendIDs = append(page.EnabledAppendIDs, id)
				}
			} else {
				chatID, _ := positiveID(page.ChatID)
				page.EnabledAppendIDs, err = kritui_db.GetChatPromptAppendIDs(r.Context(), database, chatID)
				if err != nil {
					log.Printf("get chat prompt append IDs after settings save: %v", err)
					failurePage := page
					failurePage.Saved = false
					failurePage.ErrorMessage = "Failed to load chat appends."
					renderSettingsPage(w, r, http.StatusInternalServerError, failurePage)
					return
				}
			}
		}
		render(http.StatusOK, "")
	}
}

// selectedDefaultTools returns submitted capability names after validating
// them against built-in tools and configured MCP servers. Unknown capabilities
// return an error.
func selectedDefaultTools(registry *tools.Registry, mcpServers []kritui_db.MCPServer, names []string) ([]string, error) {
	availableMCP := make(map[string]struct{}, len(mcpServers))
	for _, server := range mcpServers {
		availableMCP[kritui_db.MCPServerCapability(server.ID)] = struct{}{}
	}
	selected := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		_, isMCPServer := availableMCP[name]
		if !registry.HasCapability(name) && !isMCPServer {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		selected = append(selected, name)
	}
	return selected, nil
}

func deleteChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		current := r.URL.Query().Get("current")
		currentChatID, ok := positiveID(current)
		if !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid current chat is required.")
			return
		}

		if _, err := database.ExecContext(r.Context(), `DELETE FROM chats WHERE id = ?`, chatID); err != nil {
			log.Printf("delete chat: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to delete chat.")
			return
		}
		if err := renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize); err != nil {
			log.Printf("render chat history after delete: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Chat was deleted, but history could not be refreshed.")
			return
		}
		if chatID == currentChatID {
			if err := templates.MessageList("", nil, true).Render(r.Context(), w); err != nil {
				log.Printf("clear deleted chat: %v", err)
			}
		}
	}
}

func renameChatHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, ok := positiveID(r.PathValue("chat"))
		if !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid chat is required.")
			return
		}
		current := r.URL.Query().Get("current")
		if _, ok := positiveID(current); !ok {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A valid current chat is required.")
			return
		}

		if err := parseLimitedForm(w, r, maxRenameBodyBytes); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				renderHistoryMutationError(w, r, http.StatusRequestEntityTooLarge, "Request body is too large.")
				return
			}
			renderHistoryMutationError(w, r, http.StatusBadRequest, "Invalid rename form.")
			return
		}
		title := normalizeChatTitle(r.FormValue("title"))
		if title == "" {
			renderHistoryMutationError(w, r, http.StatusBadRequest, "A title is required.")
			return
		}

		result, err := database.ExecContext(r.Context(), `
			UPDATE chats
			SET title = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?
		`, title, chatID)
		if err != nil {
			log.Printf("rename chat: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to rename chat.")
			return
		}
		affected, err := result.RowsAffected()
		if err != nil {
			log.Printf("rename chat rows affected: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Failed to rename chat.")
			return
		}
		if affected == 0 {
			renderHistoryMutationError(w, r, http.StatusNotFound, "Chat not found.")
			return
		}

		if err := renderHistoryEntries(r.Context(), w, database, current, "", 0, defaultHistoryPageSize); err != nil {
			log.Printf("render chat history after rename: %v", err)
			renderHistoryMutationError(w, r, http.StatusInternalServerError, "Chat was renamed, but history could not be refreshed.")
		}
	}
}

func renderHistoryEntries(ctx context.Context, w http.ResponseWriter, database *sql.DB, current, beforeUpdatedAt string, beforeID int64, limit int) error {
	chats, err := kritui_db.GetChatsPage(ctx, database, beforeUpdatedAt, beforeID, limit+1)
	if err != nil {
		return fmt.Errorf("get chats: %w", err)
	}
	hasMore := len(chats) > limit
	if hasMore {
		chats = chats[:limit]
	}

	var fragment bytes.Buffer
	if err := templates.HistoryEntries(current, chats, beforeUpdatedAt == "", hasMore).Render(ctx, &fragment); err != nil {
		return fmt.Errorf("render history entries: %w", err)
	}
	if err := templates.HistoryError("", true).Render(ctx, &fragment); err != nil {
		return fmt.Errorf("render history status: %w", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(fragment.Bytes()); err != nil {
		return fmt.Errorf("write history entries: %w", err)
	}
	return nil
}

func renderPageError(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := templates.PageError(message).Render(r.Context(), &page); err != nil {
		log.Printf("render page error: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

func promptAppendsFromForm(r *http.Request) ([]kritui_db.PromptAppend, error) {
	defaultIDs := make([]string, 0, len(r.Form["default_append"]))
	defaults := make(map[string]struct{}, len(r.Form["default_append"]))
	for _, id := range r.Form["default_append"] {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := kritui_db.ValidatePromptAppendID(id); err != nil {
			return nil, err
		}
		if _, ok := defaults[id]; ok {
			continue
		}
		defaults[id] = struct{}{}
		defaultIDs = append(defaultIDs, id)
	}

	ids := r.Form["append_id"]
	values := make([]kritui_db.PromptAppend, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("prompt append ID is required")
		}
		if err := kritui_db.ValidatePromptAppendID(id); err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("prompt append %q is duplicated", id)
		}
		seen[id] = struct{}{}
		_, enabledByDefault := defaults[id]
		values = append(values, kritui_db.PromptAppend{
			ID:               id,
			Name:             r.FormValue("append_name_" + id),
			Text:             r.FormValue("append_text_" + id),
			EnabledByDefault: enabledByDefault,
		})
	}
	for _, id := range defaultIDs {
		if _, ok := seen[id]; !ok {
			return values, fmt.Errorf("unknown default prompt append %q", id)
		}
	}
	return values, nil
}

func newPromptAppendID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "append-" + hex.EncodeToString(value[:]), nil
}

func mcpServersFromForm(r *http.Request, stored []kritui_db.MCPServer) ([]kritui_db.MCPServerUpdate, error) {
	storedByID := make(map[string]kritui_db.MCPServer, len(stored))
	for _, server := range stored {
		storedByID[server.ID] = server
	}
	clearIDs := make(map[string]struct{}, len(r.Form["clear_mcp_authorization"]))
	for _, id := range r.Form["clear_mcp_authorization"] {
		id = strings.TrimSpace(id)
		if err := kritui_db.ValidateMCPServerID(id); err != nil {
			return nil, err
		}
		clearIDs[id] = struct{}{}
	}

	ids := r.Form["mcp_id"]
	updates := make([]kritui_db.MCPServerUpdate, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if err := kritui_db.ValidateMCPServerID(id); err != nil {
			return updates, err
		}
		if _, ok := seen[id]; ok {
			return updates, fmt.Errorf("MCP server %q is duplicated", id)
		}
		seen[id] = struct{}{}
		server := kritui_db.MCPServer{
			ID:   id,
			Name: r.FormValue("mcp_name_" + id),
			URL:  r.FormValue("mcp_url_" + id),
		}
		if previous, ok := storedByID[id]; ok {
			server.AuthorizationConfigured = previous.AuthorizationConfigured
		}
		value := strings.TrimSpace(r.FormValue("mcp_authorization_" + id))
		_, clear := clearIDs[id]
		update := kritui_db.MCPServerUpdate{Server: server}
		switch {
		case clear && value != "":
			updates = append(updates, update)
			return updates, fmt.Errorf("choose replacing or clearing authorization token for %q, not both", server.Name)
		case clear:
			update.AuthorizationChange = kritui_db.MCPClearAuthorization
		case value != "":
			update.AuthorizationChange = kritui_db.MCPReplaceAuthorization
			update.AuthorizationValue = value
			update.Server.AuthorizationConfigured = true
		}
		updates = append(updates, update)
	}
	for id := range clearIDs {
		if _, ok := seen[id]; !ok {
			return updates, fmt.Errorf("unknown MCP server authorization clear %q", id)
		}
	}
	return updates, nil
}

func mcpServersFromUpdates(updates []kritui_db.MCPServerUpdate) []kritui_db.MCPServer {
	servers := make([]kritui_db.MCPServer, 0, len(updates))
	for _, update := range updates {
		servers = append(servers, update.Server)
	}
	return servers
}

func mcpAuthorizationClearIDs(updates []kritui_db.MCPServerUpdate) []string {
	ids := make([]string, 0, len(updates))
	for _, update := range updates {
		if update.AuthorizationChange == kritui_db.MCPClearAuthorization {
			ids = append(ids, update.Server.ID)
		}
	}
	return ids
}

func mcpServersAfterSave(updates []kritui_db.MCPServerUpdate) []kritui_db.MCPServer {
	servers := make([]kritui_db.MCPServer, 0, len(updates))
	for _, update := range updates {
		server := update.Server
		switch update.AuthorizationChange {
		case kritui_db.MCPClearAuthorization:
			server.AuthorizationConfigured = false
		case kritui_db.MCPReplaceAuthorization:
			server.AuthorizationConfigured = true
		}
		servers = append(servers, server)
	}
	return servers
}

func newMCPServerID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "mcp-" + hex.EncodeToString(value[:]), nil
}

func renderSettingsPage(w http.ResponseWriter, r *http.Request, status int, data templates.SettingsPanelData) {
	data.Visible = true
	data.Models, data.SelectedModel = availableModels(r, data.SelectedModel)
	var page bytes.Buffer
	if err := templates.SettingsPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render settings: %v", err)
		return
	}
	if data.Saved && r.Header.Get("HX-Request") == "true" {
		if err := templates.ToolPicker(data.ToolNames, data.MCPServers, data.EnabledToolNames, true).Render(r.Context(), &page); err != nil {
			log.Printf("render tools: %v", err)
			return
		}
		if err := templates.AppendsPicker(data.PromptAppends, data.EnabledAppendIDs, true).Render(r.Context(), &page); err != nil {
			log.Printf("render prompt appends: %v", err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

func renderMCPServerEditor(w http.ResponseWriter, r *http.Request, status int, server kritui_db.MCPServer) {
	var page bytes.Buffer
	if err := templates.MCPServerEditor(server, false, false).Render(r.Context(), &page); err != nil {
		log.Printf("render MCP server editor: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

func renderHistoryLoadError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.HistoryLoadError(message).Render(r.Context(), w); err != nil {
		log.Printf("render history load error: %v", err)
	}
}

func renderHistoryMutationError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Retarget", "#history-error")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.WriteHeader(status)
	if err := templates.HistoryError(message, false).Render(r.Context(), w); err != nil {
		log.Printf("render history mutation error: %v", err)
	}
}

func historyCursorID(r *http.Request, beforeUpdatedAt string) (int64, bool) {
	value := r.URL.Query().Get("before_id")
	if beforeUpdatedAt == "" {
		return 0, value == ""
	}
	return positiveID(value)
}

func historyPageLimit(r *http.Request) (int, bool) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultHistoryPageSize, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && limit > 0 && limit <= maxHistoryPageSize
}

func positiveID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func availableModels(r *http.Request, selected string) ([]string, string) {
	selected = strings.TrimSpace(selected)
	sessionID := ""
	if id, ok := positiveID(r.URL.Query().Get("chat")); ok {
		sessionID = strconv.FormatInt(id, 10)
	}
	client, err := llm.New(os.Getenv("LLM_KEY"), selected, os.Getenv("LLM_ENDPOINT"), llm.ClientOptions{SessionID: sessionID})
	if err != nil {
		if selected == "" {
			return nil, ""
		}
		return []string{selected}, selected
	}

	models, err := client.Models(r.Context())
	if err != nil {
		log.Printf("get models: %v", err)
		return []string{selected}, selected
	}
	if slices.Contains(models, selected) {
		return models, selected
	}
	if selected == "" {
		return models, selected
	}
	return append([]string{selected}, models...), selected
}
