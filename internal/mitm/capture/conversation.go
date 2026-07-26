package capture

import "net/http"

// ConversationSource names how a captured request's conversation id was
// recovered. It is provenance, not control flow: a reader uses it to tell a
// chat turn apart from a nested subagent run that belongs to the same
// conversation, since both carry the identical ConversationID.
type ConversationSource string

const (
	// ConversationSourceUnknown marks a row whose conversation could not be
	// resolved, either because the provider has no resolver or because the
	// request carried no chat identifier. It is the zero value.
	ConversationSourceUnknown ConversationSource = ""
	// ConversationSourceCursorRunThread marks a Cursor agent run whose own
	// thread is the conversation, meaning a top-level chat turn.
	ConversationSourceCursorRunThread ConversationSource = "cursor.run.thread"
	// ConversationSourceCursorRunParent marks a Cursor agent run whose own
	// thread is a subagent; the conversation is the run's parent. Clyde does
	// not index subagent transcripts as conversations, so the parent is the
	// only id that resolves to something the conversation index holds.
	ConversationSourceCursorRunParent ConversationSource = "cursor.run.parent"
	// ConversationSourceCursorMetadata marks a Cursor conversation-metadata
	// write, which names its conversation directly.
	ConversationSourceCursorMetadata ConversationSource = "cursor.conversation_metadata"
)

// ConversationRef ties one captured request to the conversation it belongs to.
// ConversationID is clyde's derived conversation id (`cursor:<uuid>`), the same
// string the conversation index and the CLI use, so a stored row joins straight
// to a conversation without a translation step.
type ConversationRef struct {
	ConversationID string
	Source         ConversationSource
}

// Resolved reports whether the ref names a conversation.
func (r ConversationRef) Resolved() bool { return r.ConversationID != "" }

// UnresolvedConversation is the ref for a request that names no conversation.
// Returning it is how a resolver says "no match" instead of guessing.
func UnresolvedConversation() ConversationRef {
	return ConversationRef{ConversationID: "", Source: ConversationSourceUnknown}
}

// RequestConversationInput is the request half of one exchange, as a
// conversation resolver reads it. The same shape serves a request on its way
// into the store and a row read back out of it, so one extractor covers both
// and a row captured before the join column existed still resolves without a
// migration or a rewrite.
type RequestConversationInput struct {
	Path        string
	ContentType string
	Headers     http.Header
	Body        []byte
}

// ConversationRefResolver recovers the conversation a request belongs to.
// Provider packages implement it, so the capture store stays free of any
// provider's wire shape. A resolver reports false for a request outside its own
// wire contract, and the unresolved ref with true for a request that is in
// contract but names no conversation.
type ConversationRefResolver interface {
	ResolveConversation(input RequestConversationInput) (ConversationRef, bool)
}
