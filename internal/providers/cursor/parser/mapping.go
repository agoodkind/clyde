package parser

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

type composerToolStatus string

const (
	composerToolStatusError     composerToolStatus = "error"
	composerToolStatusFailed    composerToolStatus = "failed"
	composerToolStatusCancelled composerToolStatus = "cancelled"
)

func mapJSONLMessage(
	message cursorjsonl.TranscriptMessage,
	_ conversation.LoadOptions,
) (transcript.Message, bool) {
	textParts := make([]string, 0)
	tools := make([]transcript.ToolCall, 0)
	for _, part := range message.Parts {
		switch part.Type {
		case cursorjsonl.PartTypeText:
			text := part.Text
			// Clyde injects synthetic envelopes into the assistant stream
			// only, so stripping is scoped to an assistant turn. Running it
			// on a user turn would risk deleting part of what the user
			// actually wrote, e.g. a pasted transcript or source file that
			// happens to quote the marker syntax.
			if message.Role == cursorjsonl.RoleAssistant {
				text = stripClydeSyntheticEnvelopes(text)
			}
			if strings.TrimSpace(text) != "" {
				textParts = append(textParts, text)
			}
		case cursorjsonl.PartTypeToolUse:
			tools = append(tools, transcript.ToolCall{
				ID:      "",
				Name:    part.ToolName,
				Input:   transcript.ToolInputJSON{Raw: cloneRaw(part.ToolInput)},
				Output:  "",
				IsError: false,
			})
		}
	}
	text := strings.Join(textParts, "\n")
	// A turn Cursor recorded as failed or aborted, or one that ended at a boundary
	// Clyde could not read, must not reach a reader as an ordinary finished
	// answer. The transcript message model carries no turn-outcome field, so the
	// marker goes in the text, where every renderer already shows it.
	if marker := turnOutcomeMarker(message.Outcome); marker != "" {
		if text == "" {
			text = marker
		} else {
			text += "\n" + marker
		}
	}
	if text == "" && len(tools) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              message.Role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              text,
		Thinking:          "",
		HasTools:          len(tools) > 0,
		Tools:             tools,
	}, true
}

// turnOutcomeMarker renders what the transcript said about how a turn ended.
// A turn that simply ran into the next role change, or into the end of the file,
// gets no marker, because the artifact says nothing about it either way.
func turnOutcomeMarker(outcome cursorjsonl.TurnOutcome) string {
	if outcome.Uncertain {
		return "[turn boundary unreadable: this turn may be incomplete]"
	}
	if !outcome.Failed() {
		return ""
	}
	// Label falls back to the boundary kind, because Cursor's standalone error
	// event carries its reason with no status at all.
	label := outcome.Label()
	reason := strings.TrimSpace(outcome.Error)
	if reason == "" {
		return "[turn ended: " + label + "]"
	}
	return "[turn ended: " + label + " (" + reason + ")]"
}

func mapComposerBubble(
	bubble cursorstore.Bubble,
	opts conversation.LoadOptions,
) (transcript.Message, bool) {
	role := "assistant"
	if bubble.Type == cursorstore.BubbleTypeUser {
		role = "user"
	}
	tools := make([]transcript.ToolCall, 0)
	if bubble.ToolCall != nil {
		toolOutput := ""
		if opts.IncludeToolOutputs {
			toolOutput = string(bubble.ToolCall.Result)
		}
		tools = append(tools, transcript.ToolCall{
			ID:      "",
			Name:    bubble.ToolCall.Name,
			Input:   transcript.ToolInputJSON{Raw: rawArgsToJSON(bubble.ToolCall.RawArgs)},
			Output:  toolOutput,
			IsError: toolErrored(bubble.ToolCall.Status),
		})
	}
	// Cursor persists whatever Clyde's MITM/BYOK ingress streamed into
	// delta.content as an ordinary bubble, so a prior turn's synthetic
	// envelope (thinking, notice, redacted-thinking) reads back here
	// looking like genuine assistant prose unless it is stripped before
	// Clyde re-ingests its own conversation. Clyde injects only into the
	// assistant stream, so stripping requires bubble.Type to say assistant
	// explicitly, rather than merely "not user": a bubble whose type is
	// neither (a partial write, or a future value this parser does not
	// yet know) must not fall into the destructive branch by default. A
	// match on anything other than a confirmed assistant bubble is the
	// user's own text (e.g. a pasted transcript or source file quoting the
	// marker syntax), not Clyde's content, and stripping it could delete
	// part of what the user actually wrote, up to the entire message.
	text := bubble.Text
	if bubble.Type == cursorstore.BubbleTypeAssistant {
		text = stripClydeSyntheticEnvelopes(bubble.Text)
	}
	if text == "" && bubble.Thinking.Text == "" && len(tools) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              bubble.BubbleID,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         composerBubbleTimestamp(bubble.CreatedAt),
		Text:              text,
		Thinking:          bubble.Thinking.Text,
		HasTools:          len(tools) > 0,
		Tools:             tools,
	}, true
}

// composerBubbleTimestamp reads the write time Cursor stored on a bubble. The
// value is the same one that orders the chat, so a message that carries it in
// one place and not the other would be describing two different conversations,
// which is why both sides go through [cursorstore.OrderableTimestamp].
//
// A row with no timestamp keeps the zero time rather than an invented one. A row
// carrying one this build cannot parse also keeps the zero time, and says so:
// that is a value Cursor wrote and Clyde did not understand, which is worth
// knowing about and is not the same as a row that carried nothing.
func composerBubbleTimestamp(createdAt string) time.Time {
	// Ordering decides which stored values it can place a message by, so asking it
	// first is what keeps the two from disagreeing. Parsing here independently
	// would agree only by coincidence, and would diverge silently the moment
	// ordering tightened or widened what it accepts.
	orderable := cursorstore.OrderableTimestamp(createdAt)
	if orderable == "" {
		if createdAt != "" {
			slog.Debug("providers.cursor.parser.bubble_timestamp_unparsed", "concern", concern, "created_at", createdAt)
		}
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, orderable)
	if err != nil {
		slog.Debug("providers.cursor.parser.bubble_timestamp_unparsed", "concern", concern, "created_at", createdAt, "err", err)
		return time.Time{}
	}
	return parsed.UTC()
}

func mapLegacyBubble(
	bubble cursorstore.LegacyChatBubble,
	_ conversation.LoadOptions,
) (transcript.Message, bool) {
	var role string
	switch bubble.Type {
	case cursorstore.LegacyChatRoleUser:
		role = "user"
	case cursorstore.LegacyChatRoleAssistant:
		role = "assistant"
	default:
		return emptyMessage(), false
	}
	// See mapComposerBubble: Clyde injects only into the assistant stream,
	// so a user bubble is never stripped.
	text := bubble.Text
	if role == "assistant" {
		text = stripClydeSyntheticEnvelopes(bubble.Text)
	}
	if text == "" {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              text,
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}, true
}

func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

// stripClydeSyntheticEnvelopes removes every Clyde-owned synthetic content
// envelope (reasoning, notice, redacted-thinking) from Cursor-owned
// assistant message text, keeping only the surrounding prose. Kept text
// segments are joined with a blank line, because an envelope removed from
// between two prose spans leaves no separator of its own: the open marker
// carries no leading newline and the close marker's trailing blank line is
// discarded as decoration, so concatenating the surviving spans directly
// would fuse the end of one into the start of the next.
//
// Clyde's MITM proxy and BYOK ingress inject these envelopes into the
// assistant delta.content stream Cursor renders; Cursor's app then persists
// that stream verbatim as an ordinary bubble or transcript line. Left
// unstripped, a prior turn's injected envelope reads back through this
// parser as if it were genuine conversation content. [render.ExtractSyntheticParts]
// is the single parser for the marker format; this helper never re-parses
// or matches the marker text itself.
//
// Callers must apply this to assistant-authored text only. Clyde injects
// envelopes into the assistant stream alone, so a marker match on a user
// turn is never Clyde's own content; it is the user's own text (e.g. a
// pasted transcript or source file quoting the marker syntax), and
// stripping it there would delete part of what the user actually wrote.
//
// Text is unchanged unless the extractor finds a complete open-and-close
// pair somewhere in it (the single-part, same-body fast path covers both
// "no marker at all" and "a marker present but never paired"). A marker
// with no matching counterpart is equally consistent with prose that
// quotes or discusses the marker syntax as it is with a genuinely
// truncated envelope, so it is left as ordinary text rather than assumed
// to be Clyde's own content and removed.
func stripClydeSyntheticEnvelopes(text string) string {
	parts := adapterrender.ExtractSyntheticParts(text)
	textBodies := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Kind != adapterrender.SyntheticKindText {
			continue
		}
		textBodies = append(textBodies, part.Body)
	}
	return strings.Join(textBodies, "\n\n")
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawArgsToJSON(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if json.Valid([]byte(trimmed)) {
		return append(json.RawMessage(nil), []byte(trimmed)...)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return encoded
}

func toolErrored(status string) bool {
	normalizedStatus := composerToolStatus(strings.ToLower(strings.TrimSpace(status)))
	switch normalizedStatus {
	case composerToolStatusError, composerToolStatusFailed, composerToolStatusCancelled:
		return true
	default:
		return false
	}
}
