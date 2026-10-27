package templates

import "seesharpsi/kritui/llm"

// CompletionView separates the unfinished trailing tool chain from archived
// transcript messages. Stored conversation history is never changed.
type CompletionView struct {
	Messages   []llm.Message
	ToolCalls  []llm.ToolCall
	ToolErrors map[string]string
}

// ActiveCompletionView keeps trailing assistant/tool batches in the live tool
// list. A later user or final assistant message archives the preceding batches.
func ActiveCompletionView(messages []llm.Message) CompletionView {
	start := len(messages)
	for start > 0 {
		message := messages[start-1]
		if message.Role != "tool" && (message.Role != "assistant" || len(message.ToolCalls) == 0) {
			break
		}
		start--
	}
	view := CompletionView{Messages: messages[:start]}
	for _, group := range messagesForView(messages[start:]) {
		view.ToolCalls = append(view.ToolCalls, group.ToolCalls...)
		if len(group.ToolErrors) > 0 {
			if view.ToolErrors == nil {
				view.ToolErrors = make(map[string]string)
			}
			for id, message := range group.ToolErrors {
				view.ToolErrors[id] = message
			}
		}
	}
	return view
}
