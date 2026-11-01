package templates

// ModelDetail is catalog-derived metadata shown next to a model ID.
type ModelDetail struct {
	Modalities []string
}

func modelPickerSelection(value string) string {
	if value == "" {
		return "None"
	}
	return value
}
