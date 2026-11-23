package parser

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

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
	defer transcript.ContainMappingPanic()
	textParts := make([]string, 0)
	thinkingParts := make([]string, 0)
	tools := make([]transcript.ToolCall, 0)
	stamped := time.Time{}
	injected := false

	for _, part := range message.Parts {
		switch part.Type {
		case cursorjsonl.PartTypeText:
			text := part.Text
			// Users can quote Clyde's replay syntax in their own messages.
			if message.Role == cursorjsonl.RoleAssistant {
				var thinking string
				text, thinking = projectAssistantText(context.Background(), text, "")
				if thinking != "" {
					thinkingParts = append(thinkingParts, thinking)
				}
			}

			// Cursor frames a user turn in its own envelope, so the stored
			// text holds markup the user never wrote and never saw. Lifting
			// the turn's time out of that markup is what lets two identical
			// turns read as identical, which they cannot while the time sits
			// inside the text being compared.
			if message.Role == cursorjsonl.RoleUser {
				unwrapped, partStamp := cursorjsonl.UnwrapUserEnvelope(text)
				text = unwrapped
				if stamped.IsZero() {
					stamped = partStamp
				}

				// Cursor writes prompts of its own into the user role. The
				// text stays, so a transcript still shows what the model was
				// answering, and the visibility below is what keeps it out of
				// the surfaces that only want what a person said.
				if cursorjsonl.IsInjectedUserPrompt(text) {
					injected = true
				}
			}

			if strings.TrimSpace(text) != "" {
				textParts = append(textParts, text)
			}
		case cursorjsonl.PartTypeToolUse:
			input := transcript.ToolInputJSON{Raw: cloneRaw(part.ToolInput)}
			display, displayLang := toolDisplayText(part.ToolName, input)
			tools = append(
				tools,
				transcript.ToolCall{
					ID:               "",
					Name:             part.ToolName,
					Input:            input,
					Display:          display,
					DisplayLang:      displayLang,
					Output:           "",
					IsError:          false,
					Attachments:      nil,
					Timestamp:        nil,
					TimestampUnknown: true,
				},
			)
		}
	}

	// A prompt Cursor wrote into the user role is a control record: the model
	// answered it, so the answer is conversation, but the prompt is the
	// harness talking to itself. Marking it is what every consumer already
	// reads to tell those apart, and it keeps the message rather than dropping
	// it, so its answer still has a question.
	visibility := transcript.MessageVisibilityVisible
	if injected {
		visibility = transcript.MessageVisibilityMetaOnly
	}

	// Transcript renderers display incomplete turn outcomes in the message text.
	if marker := turnOutcomeMarker(message.Outcome); marker != "" {
		textParts = append(textParts, marker)
	}

	text := strings.Join(textParts, "\n")
	thinking := strings.Join(thinkingParts, "\n\n")
	if text == "" && thinking == "" && len(tools) == 0 {
		return emptyMessage(), false
	}

	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              message.Role,
		Visibility:        visibility,
		Compaction:        nil,
		Timestamp:         stamped,
		Text:              text,
		Thinking:          thinking,
		HasTools:          len(tools) > 0,
		Tools:             tools,
		Attachments:       nil,
		TextParts:         nil,
	}, true
}

// turnOutcomeMarker renders what the transcript said about how a turn ended.
// Cursor does not report an outcome when a role change or the end of the file
// ends the turn.
func turnOutcomeMarker(outcome cursorjsonl.TurnOutcome) string {
	if outcome.Uncertain {
		return turnBoundaryUnreadableMarker
	}

	if !outcome.Failed() {
		return ""
	}

	// Label falls back to the boundary kind, because Cursor's standalone error
	// event carries its reason with no status at all.
	label := outcome.Label()
	reason := strings.TrimSpace(outcome.Error)
	if reason == "" {
		return turnEndedMarkerPrefix + label + turnMarkerSuffix
	}

	return turnEndedMarkerPrefix + label + " (" + reason + ")" + turnMarkerSuffix
}

const (
	turnBoundaryUnreadableMarker = "[turn boundary unreadable: this turn may be incomplete]"
	turnEndedMarkerPrefix        = "[turn ended: "
	turnMarkerSuffix             = "]"
)

func mapComposerBubble(
	ctx context.Context,
	bubble cursorstore.Bubble,
	opts conversation.LoadOptions,
) (transcript.Message, bool) {
	defer transcript.ContainMappingPanic()
	role := cursorjsonl.RoleAssistant
	if bubble.Type == cursorstore.BubbleTypeUser {
		role = cursorjsonl.RoleUser
	}

	tools := make([]transcript.ToolCall, 0)
	if bubble.ToolCall != nil {
		toolOutput := ""
		if opts.IncludeToolOutputs {
			toolOutput = string(bubble.ToolCall.Result)
		}

		input := transcript.ToolInputJSON{Raw: rawArgsToJSON(bubble.ToolCall.RawArgs)}
		display, displayLang := toolDisplayText(bubble.ToolCall.Name, input)
		tools = append(
			tools,
			transcript.ToolCall{
				ID:               "",
				Name:             bubble.ToolCall.Name,
				Input:            input,
				Display:          display,
				DisplayLang:      displayLang,
				Output:           toolOutput,
				IsError:          toolErrored(bubble.ToolCall.Status),
				Attachments:      nil,
				Timestamp:        nil,
				TimestampUnknown: false,
			},
		)
	}

	// Clyde adds replay references and visible thinking only to the assistant
	// stream. Require an explicit assistant type before removing either.
	text := bubble.Text
	thinking := bubble.Thinking.Text
	if bubble.Type == cursorstore.BubbleTypeAssistant {
		text, thinking = projectAssistantText(ctx, text, thinking)
	}

	if text == "" && thinking == "" && len(tools) == 0 {
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
		Thinking:          thinking,
		HasTools:          len(tools) > 0,
		Tools:             tools,
		Attachments:       nil,
		TextParts:         nil,
	}, true
}

// composerBubbleTimestamp reads the write time Cursor stored on a bubble. The
// message mapper and the chat sorter both accept the same timestamps through
// [cursorstore.OrderableTimestamp].
//
// The mapper returns the zero time for a missing or invalid timestamp. It
// logs the invalid value if Cursor supplied one.
func composerBubbleTimestamp(createdAt string) time.Time {
	// Ordering decides which stored values it can place a message by, so asking it
	// first is what keeps the two from disagreeing. Parsing here independently
	// would agree only by coincidence, and would diverge silently the moment
	// ordering tightened or widened what it accepts.
	orderable := cursorstore.OrderableTimestamp(createdAt)
	if orderable == "" {
		if createdAt != "" {
			slog.Debug(
				"providers.cursor.parser.bubble_timestamp_unparsed",
				"concern",
				concern,
				"created_at",
				createdAt,
			)
		}

		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339, orderable)
	if err != nil {
		slog.Debug(
			"providers.cursor.parser.bubble_timestamp_unparsed",
			"concern",
			concern,
			"created_at",
			createdAt,
			"err",
			err,
		)

		return time.Time{}
	}

	return parsed.UTC()
}

func mapLegacyBubble(
	bubble cursorstore.LegacyChatBubble,
	_ conversation.LoadOptions,
) (transcript.Message, bool) {
	defer transcript.ContainMappingPanic()
	var role string
	switch bubble.Type {
	case cursorstore.LegacyChatRoleUser:
		role = cursorjsonl.RoleUser
	case cursorstore.LegacyChatRoleAssistant:
		role = cursorjsonl.RoleAssistant
	default:
		return emptyMessage(), false
	}

	text := bubble.Text
	thinking := ""
	if role == cursorjsonl.RoleAssistant {
		text, thinking = projectAssistantText(context.Background(), text, "")
	}

	if text == "" && thinking == "" {
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
		Thinking:          thinking,
		HasTools:          false,
		Tools:             nil,
		Attachments:       nil,
		TextParts:         nil,
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
		Attachments:       nil,
		TextParts:         nil,
	}
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
