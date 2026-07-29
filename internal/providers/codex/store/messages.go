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

func responseItemMessage(raw json.RawMessage, timestamp time.Time, includeSystemPrompts bool) (HistoryMessage, bool) {
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
			if !includeSystemPrompts {
				return emptyHistoryMessage(), false
			}
			return textHistoryMessage(roleDeveloper, text, timestamp, payload.Phase), true
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

func eventMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
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
		return textHistoryMessage("user", text, timestamp, payload.Phase), true
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
	message.Tools = []transcript.ToolCall{{
		ID:          callID,
		Name:        name,
		Input:       toolInputJSON(raw),
		Display:     "",
		DisplayLang: "",
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
