// Package semsearch defines the conversation document, search hit, filter,
// fingerprint, and job state types that the daemon and the in-process
// vectorsearch client share. It has no network code.
package semsearch

// Job states of an ingest. vectorsearch reports only JobStateCompleted, because
// an in-process upsert finishes before its call returns. Callers compare
// against all three terminal states.
const (
	JobStateCompleted = "completed"
	JobStateFailed    = "failed"
	JobStateCancelled = "cancelled"
)

// SemDoc is the conversation-message projection that vectorsearch writes as rows.
type SemDoc struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of the lineage
	// parent, or empty. Every message of one conversation has the same value.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Text                 string
	Tools                []SemToolCall
	Thinking             string
	// WorkspaceRoot is the workspace of the conversation, or empty when
	// unknown.
	WorkspaceRoot string
	Archived      bool
	// LoadRules is the tag of the loading rules that produced MessageIndex. A
	// reader with the same rules rebuilds the same message sequence.
	LoadRules string
}

// SemToolCall is one structured tool call attached to a semantic document.
type SemToolCall struct {
	Name     string
	Display  string
	LangHint string
	Output   string
	IsError  bool
}

// Fingerprint pairs a conversation id with a value that changes when the
// messages of the conversation change.
type Fingerprint struct {
	ConversationID string
	Value          string
}

// BackfillScalarEntry is the workspace root and archived status of one
// conversation for the scalar backfill.
type BackfillScalarEntry struct {
	ConversationID string
	WorkspaceRoot  string
	Archived       bool
}

// SemHit is one conversation-message match returned by the cross-conversation
// search.
type SemHit struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of the lineage
	// parent, or empty.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Content              string
	Score                float64
	// LoadRules is the loading rules tag of the matched row. An empty value
	// means the default rules.
	LoadRules string
}

// SearchFilter narrows conversation retrieval by row attributes. The zero value
// matches every row. The workspace filter uses ConversationIDs, not
// WorkspaceRoots: the workspaceRoot column is null on older rows, and every row
// has a conversation id.
type SearchFilter struct {
	Providers            []string
	WorkspaceRoots       []string
	Roles                []string
	FromUnix             int64
	UntilUnix            int64
	ConversationIDs      []string
	ParentConversationID string
	MinScore             float64
	MessageIndexFrom     int32
	MessageIndexUntil    int32
}
