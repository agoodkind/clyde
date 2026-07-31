package cursorjsonl

// Cursor writes a prompt of its own into the user role after a subagent
// finishes, so the transcript records it exactly as if the person had typed
// it. The model answers it, which makes the answer real conversation, but the
// prompt itself is Cursor instructing its own model and says nothing a reader
// or a search would want.
//
// Measured on this machine, the three texts below account for 566 user
// messages across the transcript corpus. Once the envelope is unwrapped every
// copy is byte-identical, so recognising them is exact rather than fuzzy.
//
// Matching is whole-text and never a substring: a person who quotes one of
// these sentences in their own message keeps their message. The cost of a
// missed match is one extra message; the cost of a loose match is deleting
// something the user wrote.
var injectedUserPrompts = map[string]bool{
	"Briefly inform the user about the task result and perform any follow-up actions (if needed).": true,
	"Briefly inform the user about the task result and perform any follow-up actions (if needed). " +
		"If there's no follow-ups needed, don't explicitly say that.": true,
	// The literal carries a Unicode minus sign, written as an escape so the
	// source stays ASCII and the byte sequence still matches what Cursor wrote.
	"Perform any necessary follow-up actions in response to the subagent completion above. " +
		"If no follow-up work is needed, no further action is required. " +
		"If you mention an agent or subagent in your response, link it with the `[Name](id)` " +
		"Don't use generic label such as `[agent]`, `[worker]`, or `[subagent]`. " +
		"For cloud subagents, when the agent has edited code, link to `[Review](bc-id#changes)`, " +
		"or, if you know the exact added and deleted line counts, `[Review +A −D](bc-id#changes)`, " +
		"replacing A and D with those counts. Never write A or D literally. " +
		"Use `[Try Live](bc-id#desktop)` only when the agent used computer use. " +
		"Don't repeat the same confirmation every time.": true,
}

// IsInjectedUserPrompt reports whether one unwrapped user message is a prompt
// Cursor wrote rather than the person using it.
//
// Call it with the text [UnwrapUserEnvelope] returned. Text still inside its
// envelope never matches, because the framing differs on every turn.
func IsInjectedUserPrompt(text string) bool {
	return injectedUserPrompts[text]
}
