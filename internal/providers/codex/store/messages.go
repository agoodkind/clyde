package codexstore

import (
	"encoding/json"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// Package codexstore message builders: turn a decoded rollout payload into a
// normalized HistoryMessage. The streaming layer in history.go dispatches each
// envelope to one of these constructors.

// roleDeveloper is the role Clyde assigns to all Codex system-prompt content:
// the session base instructions and the injected developer/system messages.
const roleDeveloper = "developer"

// isSystemPromptRole reports whether a response_item message role carries
// injected system-prompt guidance rather than user or assistant conversation.
func isSystemPromptRole(role string) bool {
	return role == roleDeveloper || role == "system"
}

// sessionPromptMessage surfaces the session base instructions (the Codex system
// prompt recorded in session_meta) as a developer-role message, so it renders
// and gates alongside the rest of the system-prompt content.
func sessionPromptMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	var payload sessionMetaPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	text := strings.TrimSpace(payload.BaseInstructions.Text)
	if text == "" {
		return emptyHistoryMessage(), false
	}
	return textHistoryMessage(roleDeveloper, text, timestamp, ""), true
}

func responseItemMessage(raw json.RawMessage, timestamp time.Time, opts HistoryOptions) (HistoryMessage, bool) {
	var payload responsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	switch responseItemType(payload.Type) {
	case responseItemMessageType:
		text := strings.TrimSpace(contentText(payload.Content))
		if text == "" {
			return emptyHistoryMessage(), false
		}
		// Developer and system role messages carry injected guidance (sandbox
		// permissions, developer instructions). They are system-prompt content,
		// gated behind IncludeSystemPrompts and normalized to the developer role
		// so the system role stays reserved for compaction boundaries.
		if isSystemPromptRole(payload.Role) {
			if !opts.IncludeSystemPrompts {
				return emptyHistoryMessage(), false
			}
			return textHistoryMessage(roleDeveloper, text, timestamp, payload.Phase), true
		}
		if payload.Role == "user" {
			return userTextMessage(text, timestamp, payload.Phase, opts)
		}
		return textHistoryMessage(payload.Role, text, timestamp, payload.Phase), true
	case responseItemFunctionCall:
		return toolCallHistoryMessage(payload.CallID, payload.Name, payload.Arguments, timestamp)
	case responseItemCustomToolCall:
		return toolCallHistoryMessage(payload.CallID, payload.Name, payload.Input, timestamp)
	case responseItemFunctionCallOutput, responseItemCustomToolCallOutput:
		return toolOutputHistoryMessage(payload.CallID, payload.Output, timestamp)
	case responseItemReasoning:
		// Reasoning response items carry only encrypted_content with an empty
		// summary, so the readable reasoning is surfaced from the
		// agent_reasoning event instead.
		return emptyHistoryMessage(), false
	default:
		return emptyHistoryMessage(), false
	}
}

// codexUserTextClass names what a user-role text actually is: a person's turn,
// or one of the harness shapes codex and user tooling write into the user role.
type codexUserTextClass string

const (
	codexUserTextConversation  codexUserTextClass = "conversation"
	codexUserTextSystemPrompt  codexUserTextClass = "system_prompt"
	codexUserTextSystemMessage codexUserTextClass = "system_message"
	codexUserTextInjected      codexUserTextClass = "injected"
)

// codexSystemMessageHeads open the harness frames codex writes into the user
// role: the sandbox environment block, aborted-turn markers, the approval
// request frame, and the transcript delta frame. Matching is head-anchored and
// exact, so a person quoting one of these lines mid-message keeps their
// message.
var codexSystemMessageHeads = []string{
	"<environment_context>",
	"<turn_aborted>",
	">>> APPROVAL REQUEST START",
	">>> APPROVAL REQUEST END",
	">>> TRANSCRIPT DELTA START",
	">>> TRANSCRIPT DELTA END",
	"The Codex agent has requested the following",
	"Assess the exact planned action below.",
	"Planned action JSON:",
	"The following is the Codex agent history",
}

// codexInjectedHeads open the messages user tooling pushes into the user role:
// automation heartbeats, goal context, and injected internal context.
var codexInjectedHeads = []string{
	"<codex_internal_context",
	"<heartbeat>",
	"<automation_id>",
	"<objective>",
}

// codexSystemPromptHead opens the AGENTS.md instruction message the harness
// writes into the user role: "# AGENTS.md instructions for <path>" followed by
// the <INSTRUCTIONS> block. Both parts are required so a person ASKING about
// AGENTS.md instructions ("# AGENTS.md instructions are not being applied")
// keeps their message: every generated instance in the local corpus carries
// the "for " continuation and the block, and a typed question carries neither.
const (
	codexSystemPromptHead = "# AGENTS.md instructions for "
	codexSystemPromptBody = "<INSTRUCTIONS>"
)

func classifyCodexUserText(text string) codexUserTextClass {
	if strings.HasPrefix(text, codexSystemPromptHead) && strings.Contains(text, codexSystemPromptBody) {
		return codexUserTextSystemPrompt
	}
	for _, head := range codexSystemMessageHeads {
		if strings.HasPrefix(text, head) {
			return codexUserTextSystemMessage
		}
	}
	for _, head := range codexInjectedHeads {
		if strings.HasPrefix(text, head) {
			return codexUserTextInjected
		}
	}
	return codexUserTextConversation
}

// userTextMessage gates one user-role text by what it actually is. A harness
// frame follows the matching include option; an AGENTS.md instruction message
// renders as developer-role system-prompt content so it gates and displays with
// the rest of that class; a person's turn passes through unchanged.
func userTextMessage(text string, timestamp time.Time, phase string, opts HistoryOptions) (HistoryMessage, bool) {
	switch classifyCodexUserText(text) {
	case codexUserTextSystemPrompt:
		if !opts.IncludeSystemPrompts {
			if opts.Tally != nil {
				opts.Tally.System++
			}
			return emptyHistoryMessage(), false
		}
		return textHistoryMessage(roleDeveloper, text, timestamp, phase), true
	case codexUserTextSystemMessage:
		if !opts.IncludeSystemMessages {
			if opts.Tally != nil {
				opts.Tally.System++
			}
			return emptyHistoryMessage(), false
		}
	case codexUserTextInjected:
		if !opts.IncludeInjected {
			if opts.Tally != nil {
				opts.Tally.Injected++
			}
			return emptyHistoryMessage(), false
		}
	case codexUserTextConversation:
	}
	return textHistoryMessage("user", text, timestamp, phase), true
}

func eventMessage(raw json.RawMessage, timestamp time.Time, opts HistoryOptions) (HistoryMessage, bool) {
	var payload eventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	switch eventMessageType(payload.Type) {
	case eventMessageTypeUser:
		text := strings.TrimSpace(payload.Message)
		if text == "" {
			return emptyHistoryMessage(), false
		}
		return userTextMessage(text, timestamp, payload.Phase, opts)
	case eventMessageTypeAgent:
		text := strings.TrimSpace(payload.Message)
		if text == "" {
			return emptyHistoryMessage(), false
		}
		return textHistoryMessage("assistant", text, timestamp, payload.Phase), true
	case eventMessageTypeReasoning:
		thinking := strings.TrimSpace(payload.Text)
		if thinking == "" {
			return emptyHistoryMessage(), false
		}
		message := textHistoryMessage("assistant", "", timestamp, "")
		message.Thinking = thinking
		return message, true
	default:
		return emptyHistoryMessage(), false
	}
}

// textHistoryMessage builds a chat HistoryMessage with every field set so
// exhaustruct stays satisfied; callers override the reasoning or tool fields
// afterward when they need them.
func textHistoryMessage(role, text string, timestamp time.Time, phase string) HistoryMessage {
	return HistoryMessage{
		Role:              role,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Text:              text,
		Thinking:          "",
		Tools:             nil,
		HasTools:          false,
		ToolOutputCallID:  "",
		ToolOutput:        "",
		ToolOutputIsError: false,
		Timestamp:         timestamp,
		Phase:             phase,
	}
}

// toolCallHistoryMessage builds an assistant message carrying one tool call.
// raw is the JSON arguments string (function_call) or the raw tool input
// (custom_tool_call); toolInputJSON keeps it as valid JSON for re-rendering.
func toolCallHistoryMessage(callID, name, raw string, timestamp time.Time) (HistoryMessage, bool) {
	if strings.TrimSpace(name) == "" && strings.TrimSpace(callID) == "" {
		return emptyHistoryMessage(), false
	}
	message := textHistoryMessage("assistant", "", timestamp, "")
	input := toolInputJSON(raw)
	display, displayLang := toolDisplayText(name, input)
	message.Tools = []transcript.ToolCall{{
		ID:          callID,
		Name:        name,
		Input:       input,
		Display:     display,
		DisplayLang: displayLang,
		Output:      "",
		IsError:     false,
	}}
	message.HasTools = true
	return message, true
}

// toolOutputHistoryMessage builds an output-marker record. The parser attaches
// its text to the matching tool call by call_id when tool outputs are
// requested, and drops it otherwise.
func toolOutputHistoryMessage(callID string, output json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	if strings.TrimSpace(callID) == "" {
		return emptyHistoryMessage(), false
	}
	message := textHistoryMessage("assistant", "", timestamp, "")
	message.ToolOutputCallID = callID
	message.ToolOutput = outputText(output)
	return message, true
}

// toolInputJSON keeps a tool argument payload as valid JSON. A function_call's
// arguments is already a JSON string, while a custom_tool_call's input is a
// raw string (for example a patch), so a non-JSON value is encoded as a JSON
// string.
func toolInputJSON(raw string) transcript.ToolInputJSON {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return transcript.ToolInputJSON{Raw: nil}
	}
	if json.Valid([]byte(trimmed)) {
		return transcript.ToolInputJSON{Raw: json.RawMessage(trimmed)}
	}
	encoded, err := json.Marshal(trimmed)
	if err != nil {
		return transcript.ToolInputJSON{Raw: nil}
	}
	return transcript.ToolInputJSON{Raw: json.RawMessage(encoded)}
}

// outputText renders a tool output payload as text. Codex writes the output as
// a JSON string, but an object or array payload is preserved as raw JSON.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func compactedMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	var payload compactedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	contextItems := []transcript.CompactedContextItem(nil)
	replacementHistoryCount := 0
	trimmedMessage := strings.TrimSpace(payload.Message)
	if payload.ReplacementHistory != nil {
		replacementHistoryCount = len(*payload.ReplacementHistory)
		contextItems = normalizeCompactedContextItems(*payload.ReplacementHistory)
	} else if trimmedMessage != "" {
		contextItems = []transcript.CompactedContextItem{
			legacyCompactedSummaryContextItem(trimmedMessage),
		}
	}
	return HistoryMessage{
		Role:              "system",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindBoundary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        0,
			ReplacementHistoryCount:   replacementHistoryCount,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              contextItems,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Text:              trimmedMessage,
		Thinking:          "",
		Tools:             nil,
		HasTools:          false,
		ToolOutputCallID:  "",
		ToolOutput:        "",
		ToolOutputIsError: false,
		Timestamp:         timestamp,
		Phase:             "",
	}, true
}
