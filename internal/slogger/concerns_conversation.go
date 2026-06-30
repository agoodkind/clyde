package slogger

// Conversation subsystem concern constants. The conversation index
// background refresh runs from the daemon worker but is logically a
// distinct subsystem with its own scan, load, search, and export
// pipelines. Span boundaries from `internal/conversation/index.go`
// and the conversation reader helpers land in these concern files.
const (
	ConcernConversationIndex  = "conversation.index"
	ConcernConversationScan   = "conversation.scan"
	ConcernConversationLoad   = "conversation.load"
	ConcernConversationSearch = "conversation.search"
	ConcernConversationExport = "conversation.export"
)
