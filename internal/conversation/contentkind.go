package conversation

import (
	"fmt"
	"strings"
)

// ContentKind names one transcript content type the export can select.
type ContentKind string

const (
	// ContentKindChat is conversation chat text.
	ContentKindChat ContentKind = "chat"
	// ContentKindThinking is assistant thinking or reasoning text.
	ContentKindThinking ContentKind = "thinking"
	// ContentKindToolCalls is tool invocations.
	ContentKindToolCalls ContentKind = "tool_calls"
	// ContentKindToolOutputs is tool result bodies.
	ContentKindToolOutputs ContentKind = "tool_outputs"
	// ContentKindSystemPrompts is system-injected prompts.
	ContentKindSystemPrompts ContentKind = "system_prompts"
	// ContentKindSystemMessages is provider system transcript records.
	ContentKindSystemMessages ContentKind = "system_messages"
	// ContentKindRawJSONMetadata is JSON metadata fields.
	ContentKindRawJSONMetadata ContentKind = "raw_json_metadata"
)

// AllContentKinds lists every selectable content kind in canonical render order.
var AllContentKinds = []ContentKind{
	ContentKindChat,
	ContentKindThinking,
	ContentKindToolCalls,
	ContentKindToolOutputs,
	ContentKindSystemPrompts,
	ContentKindSystemMessages,
	ContentKindRawJSONMetadata,
}

// Group selector values expand to several content kinds.
const (
	contentSelectorTools = "tools"
	contentSelectorAll   = "all"
)

// ContentKindSelectorValues lists every accepted selector value, the canonical
// kinds plus the tools and all groups, for flag and MCP enum constraints.
func ContentKindSelectorValues() []string {
	values := make([]string, 0, len(AllContentKinds)+2)
	for _, kind := range AllContentKinds {
		values = append(values, string(kind))
	}
	return append(values, contentSelectorTools, contentSelectorAll)
}

// ExpandContentSelector maps one selector value to the content kinds it covers.
// A canonical kind maps to itself, tools expands to tool_calls and tool_outputs,
// and all expands to every kind. ok is false for an unrecognized value.
func ExpandContentSelector(value string) (kinds []ContentKind, ok bool) {
	trimmed := strings.TrimSpace(value)
	switch trimmed {
	case contentSelectorTools:
		return []ContentKind{ContentKindToolCalls, ContentKindToolOutputs}, true
	case contentSelectorAll:
		return append([]ContentKind(nil), AllContentKinds...), true
	}
	for _, kind := range AllContentKinds {
		if string(kind) == trimmed {
			return []ContentKind{kind}, true
		}
	}
	return nil, false
}

// ContentKindSet is a set of content kinds with membership tests.
type ContentKindSet struct {
	members map[ContentKind]bool
}

// NewContentKindSet builds a set from the given kinds.
func NewContentKindSet(kinds ...ContentKind) ContentKindSet {
	members := make(map[ContentKind]bool, len(kinds))
	for _, kind := range kinds {
		members[kind] = true
	}
	return ContentKindSet{members: members}
}

// Has reports whether the kind is in the set. The zero set has no members.
func (s ContentKindSet) Has(kind ContentKind) bool {
	return s.members[kind]
}

// Kinds returns the members in canonical order.
func (s ContentKindSet) Kinds() []ContentKind {
	out := make([]ContentKind, 0, len(s.members))
	for _, kind := range AllContentKinds {
		if s.members[kind] {
			out = append(out, kind)
		}
	}
	return out
}

// Empty reports whether the set has no kinds.
func (s ContentKindSet) Empty() bool {
	return len(s.members) == 0
}

// ResolveContentKinds expands the selector values into a content-kind set. It
// errors on an unrecognized value or when nothing selects any kind, so an
// export always names at least one content kind.
func ResolveContentKinds(values []string) (ContentKindSet, error) {
	members := make(map[ContentKind]bool)
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		kinds, ok := ExpandContentSelector(value)
		if !ok {
			return ContentKindSet{members: nil}, fmt.Errorf("unknown content kind %q (allowed: %s)", value, strings.Join(ContentKindSelectorValues(), ", "))
		}
		for _, kind := range kinds {
			members[kind] = true
		}
	}
	if len(members) == 0 {
		return ContentKindSet{members: nil}, fmt.Errorf("no content kinds selected; pass --only or a type shortcut with one or more of: %s", strings.Join(ContentKindSelectorValues(), ", "))
	}
	return ContentKindSet{members: members}, nil
}
