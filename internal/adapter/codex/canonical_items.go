package codex

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/codexwire"
)

// continuationEvent is the canonical shape used to compare two Codex
// Responses input items for equivalence. The event extracts the
// identity-bearing fields (kind, call_id, response_id) so items that
// round-trip through canonicalization compare equal even when their
// raw representations diverge in incidental fields. Used by
// ComputeDelta in delta_input.go.
type continuationEvent struct {
	Kind     continuationKind
	Identity string
	Role     string
	Name     string
	Text     string
	Payload  string
}

// continuationKind is the closed enum of continuation comparison
// kinds.
type continuationKind string

const (
	continuationKindMessage    continuationKind = "message"
	continuationKindToolCall   continuationKind = "tool_call"
	continuationKindToolOutput continuationKind = "tool_output"
	continuationKindReasoning  continuationKind = "reasoning"
)

// continuationItemEqual reports whether two Codex input items refer
// to the same logical event. Identity-bearing items compare on
// kind+identity. Items without identity compare on canonical JSON.
func continuationItemEqual(a, b codexwire.InputItem) bool {
	aEvent, aOK := canonicalContinuationEvent(a)
	bEvent, bOK := canonicalContinuationEvent(b)
	if aOK && bOK {
		if aEvent.Identity != "" || bEvent.Identity != "" {
			return aEvent.Kind == bEvent.Kind && aEvent.Identity != "" && aEvent.Identity == bEvent.Identity
		}
		return aEvent == bEvent
	}
	return jsonEqual(canonicalContinuationItem(a), canonicalContinuationItem(b))
}

func canonicalContinuationEvent(item codexwire.InputItem) (continuationEvent, bool) {
	switch item.Type {
	case codexwire.ItemTypeMessage:
		return continuationEvent{
			Kind:     continuationKindMessage,
			Role:     strings.ToLower(strings.TrimSpace(item.Role)),
			Text:     continuationContentText(item.Content),
			Identity: "", Name: "", Payload: "",
		}, true
	case codexwire.ItemTypeFunctionCall:
		// The tool name and arguments are opaque content; the arguments
		// compare by canonical JSON, never by interpreting tool-specific
		// argument field aliases.
		return continuationEvent{
			Kind:     continuationKindToolCall,
			Identity: strings.TrimSpace(item.CallID),
			Name:     strings.TrimSpace(item.Name),
			Payload:  canonicalContinuationString(item.Arguments), Role: "", Text: "",
		}, true
	case codexwire.ItemTypeLocalShellCall:
		// local_shell_call is a codex-internal item TYPE, so the canonical
		// name is the type itself rather than any client tool name.
		return continuationEvent{
			Kind:     continuationKindToolCall,
			Identity: strings.TrimSpace(item.CallID),
			Name:     string(codexwire.ItemTypeLocalShellCall),
			Payload:  canonicalContinuationLocalShellAction(item.Action), Role: "", Text: "",
		}, true
	case codexwire.ItemTypeCustomToolCall:
		return continuationEvent{
			Kind:     continuationKindToolCall,
			Identity: strings.TrimSpace(item.CallID),
			Name:     strings.TrimSpace(item.Name),
			Payload:  item.Input, Role: "", Text: "",
		}, true
	case codexwire.ItemTypeFunctionCallOutput, codexwire.ItemTypeCustomToolCallOutput:
		return continuationEvent{
			Kind:     continuationKindToolOutput,
			Identity: strings.TrimSpace(item.CallID),
			Text:     codexwire.OutputText(item.Output), Role: "", Name: "", Payload: "",
		}, true
	case codexwire.ItemTypeReasoning:
		return continuationEvent{
			Kind:     continuationKindReasoning,
			Identity: strings.TrimSpace(item.ID),
			Text:     continuationReasoningText(item),
			Payload:  item.EncryptedContent, Role: "", Name: "",
		}, true
	default:
		return continuationEvent{Kind: "", Identity: "", Role: "", Name: "", Text: "", Payload: ""}, false
	}
}

// canonicalContinuationItem is the JSON-encodable canonical view of
// one input item. Used as a fallback equality basis when the item
// has no identity field.
type canonicalContinuationItemView struct {
	Type      codexwire.ItemType             `json:"type"`
	Role      string                         `json:"role,omitempty"`
	Text      string                         `json:"text,omitempty"`
	CallID    string                         `json:"call_id,omitempty"`
	Name      string                         `json:"name,omitempty"`
	Arguments string                         `json:"arguments,omitempty"`
	Input     string                         `json:"input,omitempty"`
	Output    string                         `json:"output,omitempty"`
	Action    *canonicalLocalShellActionView `json:"action,omitempty"`
}

// canonicalLocalShellActionView is the JSON-encodable view of a
// local_shell_call action. Empty when the action raw payload is
// empty or fails to decode.
type canonicalLocalShellActionView struct {
	Command string `json:"command,omitempty"`
	Workdir string `json:"workdir,omitempty"`
}

func canonicalContinuationItem(item codexwire.InputItem) canonicalContinuationItemView {
	out := canonicalContinuationItemView{
		Type:      item.Type,
		Role:      "",
		Text:      "",
		CallID:    "",
		Name:      "",
		Arguments: "",
		Input:     "",
		Output:    "",
		Action:    nil,
	}
	switch item.Type {
	case codexwire.ItemTypeMessage:
		out.Role = strings.ToLower(strings.TrimSpace(item.Role))
		out.Text = continuationContentText(item.Content)
	case codexwire.ItemTypeFunctionCall:
		out.CallID = strings.TrimSpace(item.CallID)
		out.Name = strings.TrimSpace(item.Name)
		out.Arguments = canonicalContinuationString(item.Arguments)
	case codexwire.ItemTypeFunctionCallOutput:
		out.CallID = strings.TrimSpace(item.CallID)
		out.Output = codexwire.OutputText(item.Output)
	case codexwire.ItemTypeCustomToolCall:
		out.CallID = strings.TrimSpace(item.CallID)
		out.Name = strings.TrimSpace(item.Name)
		out.Input = item.Input
	case codexwire.ItemTypeCustomToolCallOutput:
		out.CallID = strings.TrimSpace(item.CallID)
		out.Name = strings.TrimSpace(item.Name)
		out.Output = codexwire.OutputText(item.Output)
	case codexwire.ItemTypeLocalShellCall:
		out.CallID = strings.TrimSpace(item.CallID)
		out.Action = &canonicalLocalShellActionView{
			Command: localShellCommandText(item.Action),
			Workdir: localShellWorkdir(item.Action),
		}
	case codexwire.ItemTypeReasoning:
		// Reasoning items canonicalize on identity (id), no field
		// view captured here; the equality path is the event
		// comparison above.
	}
	return out
}

func continuationContentText(items codexwire.ContentItems) string {
	if text := items.Text(); text != "" {
		return SanitizeForUpstreamCache(text)
	}
	return ""
}

func continuationReasoningText(item codexwire.InputItem) string {
	if len(item.Summary) == 0 {
		return ""
	}
	parts := make([]string, 0, len(item.Summary))
	for _, summary := range item.Summary {
		if summary.Text != "" {
			parts = append(parts, summary.Text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return SanitizeForUpstreamCache(strings.Join(parts, "\n"))
}

func canonicalContinuationString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var decoded json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return raw
	}
	return string(encoded)
}

// canonicalShellArgumentsView is the JSON shape used for canonical
// equality of a codex native local_shell_call action.
type canonicalShellArgumentsView struct {
	Command   string `json:"command,omitempty"`
	Workdir   string `json:"workdir,omitempty"`
	TimeoutMs *int64 `json:"timeout_ms,omitempty"`
}

func canonicalContinuationLocalShellAction(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var action codexwire.LocalShellAction
	if err := json.Unmarshal(raw, &action); err != nil {
		return ""
	}
	view := canonicalShellArgumentsView{Command: "", Workdir: "", TimeoutMs: nil}
	if command := CommandString(action.Command); command != "" {
		view.Command = command
	}
	if workdir := action.WorkingDir(); workdir != "" {
		view.Workdir = workdir
	}
	if action.TimeoutMs != nil {
		ms := *action.TimeoutMs
		view.TimeoutMs = &ms
	} else if action.BlockUntilMs != nil {
		ms := *action.BlockUntilMs
		view.TimeoutMs = &ms
	}
	return canonicalContinuationJSON(view)
}

func localShellCommandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var action codexwire.LocalShellAction
	if err := json.Unmarshal(raw, &action); err != nil {
		return ""
	}
	return CommandString(action.Command)
}

func localShellWorkdir(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var action codexwire.LocalShellAction
	if err := json.Unmarshal(raw, &action); err != nil {
		return ""
	}
	return action.WorkingDir()
}

func canonicalContinuationJSON(value canonicalShellArgumentsView) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func jsonEqual(a, b canonicalContinuationItemView) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}
