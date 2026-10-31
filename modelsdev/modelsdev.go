// Package modelsdev resolves provider model metadata and pricing from the
// models.dev catalog.
package modelsdev

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// DefaultURL is the public models.dev provider catalog.
const DefaultURL = "https://models.dev/api.json"

// Modalities lists the input and output forms a model accepts.
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Limit describes a model's context and token limits in tokens.
type Limit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

// ReasoningOption describes one reasoning-control mechanism. Type is one of
// toggle, effort, or budget_tokens; effort options carry allowed Values.
type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

// CostTier is a context-size price tier, in USD per million tokens.
type CostTier struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier"`
}

// Cost contains a provider model's prices in USD per million tokens. Tiers and
// ContextOver200K describe context-size dependent pricing.
type Cost struct {
	Input           *float64   `json:"input"`
	Output          *float64   `json:"output"`
	CacheRead       *float64   `json:"cache_read"`
	CacheWrite      *float64   `json:"cache_write"`
	Reasoning       *float64   `json:"reasoning"`
	InputAudio      *float64   `json:"input_audio"`
	OutputAudio     *float64   `json:"output_audio"`
	Tiers           []CostTier `json:"tiers"`
	ContextOver200K *Cost      `json:"context_over_200k"`
}

// Model contains provider-specific model metadata from models.dev.
type Model struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Family           string            `json:"family"`
	Attachment       bool              `json:"attachment"`
	Reasoning        bool              `json:"reasoning"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options"`
	ToolCall         bool              `json:"tool_call"`
	StructuredOutput *bool             `json:"structured_output"`
	Temperature      *bool             `json:"temperature"`
	Knowledge        string            `json:"knowledge"`
	ReleaseDate      string            `json:"release_date"`
	LastUpdated      string            `json:"last_updated"`
	OpenWeights      bool              `json:"open_weights"`
	Status           string            `json:"status"`
	CanonicalModelID string            `json:"canonical_model_id"`
	Modalities       Modalities        `json:"modalities"`
	Limit            Limit             `json:"limit"`
	Cost             *Cost             `json:"cost"`
}

// Provider contains one models.dev provider and its model map.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	API    string           `json:"api"`
	NPM    string           `json:"npm"`
	Env    []string         `json:"env"`
	Doc    string           `json:"doc"`
	Models map[string]Model `json:"models"`
}

// ProviderModel is one provider model resolved from an endpoint and model ID.
type ProviderModel struct {
	ProviderID string
	Provider   Provider
	ModelID    string
	Model      Model
}

// SupportsInput reports whether the model accepts one input kind and whether
// the catalog actually declares its input modalities. Kind can be text, image,
// pdf, audio, or video, including common MIME spellings.
func (m Model) SupportsInput(kind string) (supported bool, known bool) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if len(m.Modalities.Input) == 0 {
		return false, false
	}
	for _, modality := range m.Modalities.Input {
		modality = strings.ToLower(strings.TrimSpace(modality))
		switch kind {
		case "image":
			if modality == "image" || strings.HasPrefix(modality, "image/") {
				return true, true
			}
		case "pdf":
			switch modality {
			case "pdf", "application/pdf", "file", "document":
				return true, true
			}
		case "text":
			if modality == "text" || strings.HasPrefix(modality, "text/") {
				return true, true
			}
		case "audio":
			if modality == "audio" || strings.HasPrefix(modality, "audio/") {
				return true, true
			}
		case "video":
			if modality == "video" || strings.HasPrefix(modality, "video/") {
				return true, true
			}
		default:
			if modality == kind || strings.HasPrefix(modality, kind+"/") {
				return true, true
			}
		}
	}
	return false, true
}

// SupportsImage reports whether the model declares image input support.
func (m Model) SupportsImage() (bool, bool) {
	return m.SupportsInput("image")
}

// SupportsPDF reports whether the model declares PDF or document input support.
func (m Model) SupportsPDF() (bool, bool) {
	return m.SupportsInput("pdf")
}

func (m Model) hasTextOutput() bool {
	if len(m.Modalities.Output) == 0 {
		return true
	}
	for _, modality := range m.Modalities.Output {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if modality == "text" || strings.HasPrefix(modality, "text/") {
			return true
		}
	}
	return false
}

// ReasoningEfforts returns the allowed effort values in catalog order without
// duplicates.
func (m Model) ReasoningEfforts() []string {
	efforts := []string{}
	for _, option := range m.ReasoningOptions {
		if strings.ToLower(strings.TrimSpace(option.Type)) != "effort" {
			continue
		}
		for _, value := range option.Values {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "" && !slices.Contains(efforts, value) {
				efforts = append(efforts, value)
			}
		}
	}
	return efforts
}

// SupportsReasoningToggle reports whether the model exposes a reasoning toggle.
func (m Model) SupportsReasoningToggle() bool {
	if !m.Reasoning {
		return false
	}
	return anyReasoningOption(m.ReasoningOptions, "toggle")
}

// SupportsReasoningBudget reports whether the model exposes a token budget.
func (m Model) SupportsReasoningBudget() bool {
	if !m.Reasoning {
		return false
	}
	return anyReasoningOption(m.ReasoningOptions, "budget_tokens")
}

func anyReasoningOption(options []ReasoningOption, kind string) bool {
	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option.Type), kind) {
			return true
		}
	}
	return false
}

// ValidateReasoningEffort checks one effort value against the catalog's
// reasoning options.
func (m Model) ValidateReasoningEffort(effort string) error {
	efforts := m.ReasoningEfforts()
	if len(efforts) == 0 {
		return errors.New("modelsdev: model exposes no reasoning effort options")
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if !slices.Contains(efforts, effort) {
		return fmt.Errorf("modelsdev: reasoning effort %q is not supported (allowed: %s)", effort, strings.Join(efforts, ", "))
	}
	return nil
}

// EstimateCost returns an estimated USD cost for one request. InputTokens is
// the total prompt token count, including cached and cache-write tokens.
// OutputTokens includes reasoning tokens. The boolean is false when pricing is
// absent or incomplete.
func (m Model) EstimateCost(inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int) (float64, bool) {
	if m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil {
		return 0, false
	}
	inputTokens = max(inputTokens, 0)
	cachedInputTokens = max(cachedInputTokens, 0)
	cacheWriteTokens = max(cacheWriteTokens, 0)
	outputTokens = max(outputTokens, 0)

	inputPrice := *m.Cost.Input
	outputPrice := *m.Cost.Output
	cacheReadPrice := inputPrice
	cacheWritePrice := inputPrice
	if m.Cost.CacheRead != nil {
		cacheReadPrice = *m.Cost.CacheRead
	}
	if m.Cost.CacheWrite != nil {
		cacheWritePrice = *m.Cost.CacheWrite
	}

	if tier, ok := m.Cost.tierForInput(inputTokens); ok {
		if tier.Input != nil {
			inputPrice = *tier.Input
		}
		if tier.Output != nil {
			outputPrice = *tier.Output
		}
		if tier.CacheRead != nil {
			cacheReadPrice = *tier.CacheRead
		} else if tier.Input != nil {
			cacheReadPrice = *tier.Input
		}
		if tier.CacheWrite != nil {
			cacheWritePrice = *tier.CacheWrite
		} else if tier.Input != nil {
			cacheWritePrice = *tier.Input
		}
	}

	nonCachedInput := max(inputTokens-cachedInputTokens-cacheWriteTokens, 0)
	cost := (float64(nonCachedInput)*inputPrice +
		float64(cachedInputTokens)*cacheReadPrice +
		float64(cacheWriteTokens)*cacheWritePrice +
		float64(outputTokens)*outputPrice) / 1_000_000
	return cost, true
}

func (c *Cost) tierForInput(inputTokens int) (CostTier, bool) {
	if c == nil {
		return CostTier{}, false
	}
	var selected CostTier
	found := false
	for _, tier := range c.Tiers {
		if tier.Tier.Size > 0 && inputTokens > tier.Tier.Size {
			if !found || tier.Tier.Size > selected.Tier.Size {
				selected = tier
				found = true
			}
		}
	}
	if found {
		return selected, true
	}
	if c.ContextOver200K != nil && inputTokens > 200_000 {
		return CostTier{
			Input:      c.ContextOver200K.Input,
			Output:     c.ContextOver200K.Output,
			CacheRead:  c.ContextOver200K.CacheRead,
			CacheWrite: c.ContextOver200K.CacheWrite,
		}, true
	}
	return CostTier{}, false
}
