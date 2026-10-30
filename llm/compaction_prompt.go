package llm

// defaultCompactionPrompt is the built-in instruction used when summarizing
// older tool results for a provider request.
const defaultCompactionPrompt = "Summarize the tool result concisely. Preserve important facts, values, and errors. Do not invent information. Return summary text only."

// DefaultCompactionPrompt returns the built-in tool-result summarization
// instruction. Accounts may replace it with their own prompt.
func DefaultCompactionPrompt() string {
	return defaultCompactionPrompt
}
