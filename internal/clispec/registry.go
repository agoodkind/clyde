package clispec

// NewConversationRegistry returns the registry of the conversation operations
// that render to both the terminal and MCP. The terminal entry point adds the
// hand-written operational commands to this registry before rendering; the MCP
// entry point renders it directly.
func NewConversationRegistry() *Registry {
	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, listConversationsOp())
	Register(reg, getConversationOp())
	Register(reg, getContextOp())
	Register(reg, searchConversationsOp())
	Register(reg, searchConversationOp())
	Register(reg, searchStatusOp())
	Register(reg, searchCancelOp())
	Register(reg, analyzeResultsOp())
	Register(reg, exportTranscriptOp())
	return reg
}
