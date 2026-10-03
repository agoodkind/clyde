// Package semsearch defines the conversation document, search hit, filter,
// fingerprint, and job state types that the daemon and the in-process
// vectorsearch client share. It has no network code.
package semsearch

// Job state strings that vectorsearch reports for an ingest. An in-process
// upsert finishes before its call returns, so vectorsearch reports only the
// completed state. The failed and cancelled states stay for callers that
// compare against every terminal state.
const (
	// JobStateCompleted is the terminal success state.
	JobStateCompleted = "completed"
	// JobStateFailed is the terminal failure state.
	JobStateFailed = "failed"
	// JobStateCancelled is the terminal cancellation state.
	JobStateCancelled = "cancelled"
)

// SemDoc is the conversation-message projection that vectorsearch writes as rows.
type SemDoc struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of this conversation's
	// lineage parent, or "" when the conversation has no resolvable parent. It is
	// the same for every message of one conversation so forks group with parents
	// in the index.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Text                 string
	Tools                []SemToolCall
	Thinking             string
	// WorkspaceRoot is the conversation's workspace, stored as a filterable scalar
	// column. The same for every message of one conversation; empty when unknown.
	WorkspaceRoot string
	// Archived is the conversation's archived status, stored as a filterable
	// scalar column. The same for every message of one conversation.
	Archived bool
	// LoadRules is the opaque loading-rules tag naming the rules that produced
	// MessageIndex, stored per row so a reader can rebuild the same message
	// sequence. The same for every document of one delivery.
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

// Fingerprint pairs a conversation id with a content fingerprint that changes
// whenever the conversation's messages change. The sync pass states the full set
// each time, and vectorsearch compares it with the recorded fingerprints to find
// the conversations that need documents.
type Fingerprint struct {
	ConversationID string
	Value          string
}

// BackfillScalarEntry is the enrichment for one conversation in the scalar
// backfill: the workspace root and archived status clyde observed, keyed by
// conversation id. The backfill writes these onto rows with an empty
// workspaceRoot and keeps each row's vector.
type BackfillScalarEntry struct {
	ConversationID string
	WorkspaceRoot  string
	Archived       bool
}

// SemHit is one conversation-message match returned by the cross-conversation
// search.
type SemHit struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of the matched
	// conversation's lineage parent, or "" when it has no resolvable parent.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Content              string
	// Score is the retrieval relevance.
	Score float64
	// LoadRules is the loading-rules tag stored with the matched row. Empty on
	// rows written before tagging existed, which readers treat as the default
	// rules.
	LoadRules string
}

// SearchFilter narrows conversation retrieval by row attributes. Every field
// is optional; the zero value matches everything. Providers filter natively on
// the provider column. WorkspaceRoots maps to the workspace column but is unused
// for the workspace filter today: workspace_root is null on rows indexed before
// that column existed, so clyde instead resolves a workspace prefix to the
// matching ConversationIDs, and every row has a conversation id. ConversationIDs
// scopes to explicit conversations (a positional conversation id or a
// within-search) and to that resolved workspace set.
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
