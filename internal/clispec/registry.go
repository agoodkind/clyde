package clispec

// NewConversationRegistry returns the shared clispec registry for the
// conversation surface plus any registry-owned operational commands. Some
// operations render to both the terminal and MCP, while CLI-only operations are
// still registered here because RenderMCP skips them when building the tool
// surface.
func NewConversationRegistry() *Registry {
	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, searchOp())
	Register(reg, conversationInfoOp())
	Register(reg, exportTranscriptOp())
	Register(reg, reorientOp())
	Register(reg, logsInventoryOp())
	return reg
}
