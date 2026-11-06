package templates

import (
	"slices"
	"strings"
)

// ModelDetail is catalog-derived metadata shown next to a model ID.
type ModelDetail struct {
	Modalities       []string
	ReasoningEfforts []string
}

// SelectedReasoningEffort resets incompatible selections to provider Default.
func SelectedReasoningEffort(detail ModelDetail, effort string) string {
	if slices.Contains(detail.ReasoningEfforts, effort) {
		return effort
	}
	return ""
}

func thinkingLabel(effort string) string {
	if effort == "" {
		return "Default"
	}
	if effort == "xhigh" {
		return "Extra high"
	}
	return strings.ToUpper(effort[:1]) + effort[1:]
}

func modelPickerSelection(value string) string {
	if value == "" {
		return "None"
	}
	return value
}
