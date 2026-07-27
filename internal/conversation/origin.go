package conversation

// Origin names who started a conversation. A working session that dispatches
// agents writes one provider transcript per agent run, and both Claude and Codex
// mark those artifacts, so the classification is a read of provider-owned data
// rather than a guess.
//
// It is a named type rather than a boolean so a third origin can be added
// without touching every caller. The parent conversation of a subagent run,
// where the provider supplies one, is carried by [Lineage] as a spawn
// relationship rather than duplicated on the origin.
type Origin string

const (
	// OriginUnspecified means the artifact's origin was not classified. It is the
	// value an index cache written before origin classification decodes to, and
	// the value a provider parser that does not read an origin marker reports. An
	// unspecified record is never treated as a subagent conversation.
	OriginUnspecified Origin = ""
	// OriginUser means a person started the conversation directly.
	OriginUser Origin = "user"
	// OriginSubagent means a parent conversation dispatched an agent that wrote
	// this transcript. Codex names the parent thread; Claude supplies no parent.
	OriginSubagent Origin = "subagent"
)

// IsSubagent reports whether the record was written by a dispatched agent rather
// than by a person. It is the one predicate the index policy consults, so the
// meaning of "subagent conversation" is declared here and nowhere else.
func (r *Record) IsSubagent() bool {
	return r.Origin == OriginSubagent
}
