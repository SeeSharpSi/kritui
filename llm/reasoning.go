package llm

import (
	"errors"
	"strings"
)

// ValidateReasoningEffort bounds stored and submitted effort values. Model-
// specific support is checked separately against the models.dev catalog.
func ValidateReasoningEffort(effort string) error {
	if len(effort) > 32 || strings.IndexFunc(effort, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return errors.New("thinking level must be an identifier of at most 32 bytes")
	}
	return nil
}

type reasoningEffortConfig struct {
	Effort string `json:"effort"`
}

func (c *Client) reasoningConfig() *reasoningEffortConfig {
	if c.reasoningEffort == "" {
		return nil
	}
	return &reasoningEffortConfig{Effort: c.reasoningEffort}
}
