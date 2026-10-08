package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ToolResultElisionMode selects how tool-result content is shaped for provider
// requests. Canonical conversation history remains unchanged.
type ToolResultElisionMode string

const (
	ToolResultElisionNone          ToolResultElisionMode = "none"
	ToolResultElisionCurrentTurn   ToolResultElisionMode = "current_turn"
	ToolResultElisionLastUserTurns ToolResultElisionMode = "last_user_turns"
	ToolResultElisionBudget        ToolResultElisionMode = "tool_result_budget"
	ToolResultElisionSummarize     ToolResultElisionMode = "summarize"
	MaxElisionUserTurns                                  = 1000
	MaxElisionTokenBudget                                = 10000000
)

const (
	elidedToolResultMarker     = "[Tool result elided]"
	summarizedToolResultMarker = "[Tool result summarized]\n"
)

// ToolResultElisionConfig controls request-only reduction of tool-result
// bodies. TokenBudget uses an approximate ceil(UTF-8 bytes / 4) count; marker
// costs and the preserved trailing fresh-result batch form an irreducible
// floor.
type ToolResultElisionConfig struct {
	Mode         ToolResultElisionMode
	UserTurns    int
	TokenBudget  int
	SummaryModel string
}

// Validate checks only parameters used by the selected mode. Empty Mode is
// equivalent to ToolResultElisionNone.
func (config ToolResultElisionConfig) Validate() error {
	switch config.Mode {
	case "", ToolResultElisionNone, ToolResultElisionCurrentTurn:
		return nil
	case ToolResultElisionLastUserTurns:
		if config.UserTurns < 1 || config.UserTurns > MaxElisionUserTurns {
			return fmt.Errorf("llm: tool-result elision user turns must be between 1 and %d", MaxElisionUserTurns)
		}
	case ToolResultElisionBudget:
		if config.TokenBudget < 1 || config.TokenBudget > MaxElisionTokenBudget {
			return fmt.Errorf("llm: tool-result token budget must be between 1 and %d", MaxElisionTokenBudget)
		}
	case ToolResultElisionSummarize:
		model := strings.TrimSpace(config.SummaryModel)
		if model == "" {
			return errors.New("llm: tool-result summary model is required")
		}
		if len(model) > 512 {
			return errors.New("llm: tool-result summary model must be at most 512 bytes")
		}
	default:
		return fmt.Errorf("llm: unsupported tool-result elision mode %q", config.Mode)
	}
	return nil
}

func (c *Conversation) shapeToolResults(ctx context.Context) ([]Message, error) {
	canonical := c.messages
	shaped := cloneMessages(canonical)
	config := c.toolResultElision
	freshStart := trailingToolBatchStart(canonical)
	eligible := eligibleToolResults(canonical, freshStart, config)

	switch config.Mode {
	case ToolResultElisionCurrentTurn, ToolResultElisionLastUserTurns:
		for _, index := range eligible {
			shaped[index].Content = elidedToolResultMarker
		}
	case ToolResultElisionBudget:
		applyToolResultBudget(canonical, shaped, eligible, config.TokenBudget)
	case ToolResultElisionSummarize:
		if c.summaryCache == nil {
			c.summaryCache = make(map[int]string)
		}
		for _, index := range eligible {
			summary, ok := c.summaryCache[index]
			if !ok {
				var err error
				summary, err = c.summarizeToolResult(ctx, canonical, index)
				if err != nil {
					return nil, err
				}
				c.summaryCache[index] = summary
			}
			if len(summary) <= len(canonical[index].Content) {
				shaped[index].Content = summary
			}
		}
	default:
		return nil, fmt.Errorf("llm: unsupported tool-result elision mode %q", config.Mode)
	}
	return shaped, nil
}

func trailingToolBatchStart(messages []Message) int {
	index := len(messages)
	for index > 0 && messages[index-1].Role == "tool" {
		index--
	}
	return index
}

func eligibleToolResults(messages []Message, freshStart int, config ToolResultElisionConfig) []int {
	cutoff := -1
	switch config.Mode {
	case ToolResultElisionCurrentTurn:
		cutoff = latestUserMessage(messages)
	case ToolResultElisionLastUserTurns:
		users := userMessageIndexes(messages)
		if len(users) < config.UserTurns {
			return nil
		}
		cutoff = users[len(users)-config.UserTurns]
	case ToolResultElisionSummarize:
		cutoff = latestUserMessage(messages)
	case ToolResultElisionBudget:
		cutoff = len(messages)
	default:
		return nil
	}
	if cutoff < 0 {
		return nil
	}

	indexes := make([]int, 0)
	for index := 0; index < len(messages) && index < freshStart; index++ {
		if messages[index].Role == "tool" && index < cutoff {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func latestUserMessage(messages []Message) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			return index
		}
	}
	return -1
}

func userMessageIndexes(messages []Message) []int {
	indexes := make([]int, 0)
	for index, message := range messages {
		if message.Role == "user" {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func applyToolResultBudget(canonical, shaped []Message, eligible []int, budget int) {
	total := int64(0)
	for _, message := range canonical {
		if message.Role == "tool" {
			total += estimateToolResultTokens(message.Content)
		}
	}
	markerCost := estimateToolResultTokens(elidedToolResultMarker)
	for _, index := range eligible {
		if total <= int64(budget) {
			return
		}
		originalCost := estimateToolResultTokens(canonical[index].Content)
		if markerCost >= originalCost {
			continue
		}
		shaped[index].Content = elidedToolResultMarker
		total += markerCost - originalCost
	}
}

// estimateToolResultTokens uses ceil(UTF-8 byte length / 4). The marker and
// preserved fresh-result batch cannot be reduced, so either can keep total
// above the configured budget.
func estimateToolResultTokens(content string) int64 {
	bytes := int64(len(content))
	tokens := bytes / 4
	if bytes%4 != 0 {
		tokens++
	}
	return tokens
}

func (c *Conversation) summarizeToolResult(ctx context.Context, messages []Message, index int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.summarizer == nil {
		return "", errors.New("llm: tool-result summarizer client is required")
	}
	functionName, arguments := toolCallContext(messages, index)
	var prompt strings.Builder
	if functionName != "" {
		fmt.Fprintf(&prompt, "Tool function: %s\n", functionName)
	}
	if arguments != "" {
		fmt.Fprintf(&prompt, "Tool arguments: %s\n", arguments)
	}
	prompt.WriteString("Tool result:\n")
	prompt.WriteString(messages[index].Content)

	completion, err := c.summarizer.complete(ctx, []Message{
		{Role: "system", Content: "Summarize the tool result concisely. Preserve important facts, values, and errors. Do not invent information. Return summary text only."},
		{Role: "user", Content: prompt.String()},
	}, nil)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if completion.Role != "assistant" || strings.TrimSpace(completion.Content) == "" || len(completion.ToolCalls) != 0 {
		return "", errors.New("llm: tool-result summarizer must return nonempty assistant text without tool calls")
	}
	return summarizedToolResultMarker + strings.TrimSpace(completion.Content), nil
}

func toolCallContext(messages []Message, toolIndex int) (string, string) {
	toolCallID := messages[toolIndex].ToolCallID
	for index := toolIndex - 1; index >= 0; index-- {
		if messages[index].Role != "assistant" {
			continue
		}
		for _, call := range messages[index].ToolCalls {
			if call.ID == toolCallID {
				return call.Function.Name, call.Function.Arguments
			}
		}
	}
	return "", ""
}
