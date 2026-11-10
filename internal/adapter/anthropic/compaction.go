package anthropic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/adapter/content"
)

// SegmentKind is the content kind of one countable piece of a compaction
// request.
type SegmentKind uint8

const (
	// SegmentText is an assistant or user text block.
	SegmentText SegmentKind = iota
	// SegmentThinking is a thinking or redacted_thinking block.
	SegmentThinking
	// SegmentToolUse is a tool call.
	SegmentToolUse
	// SegmentToolResult is the answer to a tool call.
	SegmentToolResult
	// SegmentImage is an image block.
	SegmentImage
	// SegmentOther is a block kind this package does not classify.
	SegmentOther
)

// Segment is one content block. Text is what a token counter measures.
type Segment struct {
	Kind SegmentKind
	Text string
	// ToolUseID is the call id on a tool call and on the result that answers
	// it. It is empty on every other kind.
	ToolUseID string
}

// CompactionRole is the role of one wire message. Anthropic accepts user and
// assistant; Claude Code also sends its system reminders as wire messages.
type CompactionRole string

const (
	// CompactionRoleUser is a message the client sent.
	CompactionRoleUser CompactionRole = "user"
	// CompactionRoleAssistant is a message the model produced.
	CompactionRoleAssistant CompactionRole = "assistant"
	// CompactionRoleSystem is a message that exists only on the wire.
	CompactionRoleSystem CompactionRole = "system"
	// CompactionRoleOther is a role this package does not name.
	CompactionRoleOther CompactionRole = "other"
)

func compactionRole(wire string) CompactionRole {
	switch CompactionRole(wire) {
	case CompactionRoleUser:
		return CompactionRoleUser
	case CompactionRoleAssistant:
		return CompactionRoleAssistant
	case CompactionRoleSystem:
		return CompactionRoleSystem
	case CompactionRoleOther:
		return CompactionRoleOther
	}
	return CompactionRoleOther
}

// CompactionMessage is one wire message of a compaction request.
type CompactionMessage struct {
	Role     CompactionRole
	Segments []Segment
}

// CompactionRequest is an intercepted compaction request, decoded into the
// pieces a token counter measures. It reads the request bytes alone.
type CompactionRequest struct {
	// SessionID is the Claude session id from metadata.user_id.
	SessionID string
	Messages  []CompactionMessage
}

type compactionWireRequest struct {
	Messages []compactionWireMessage `json:"messages"`
	Metadata *compactionMetadata     `json:"metadata"`
}

type compactionWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type compactionMetadata struct {
	UserID string `json:"user_id"`
}

// compactionUserID decodes metadata.user_id, which is a JSON string that itself
// contains an encoded JSON object.
type compactionUserID struct {
	SessionID string `json:"session_id"`
}

// DecodeCompactionRequest decodes an intercepted /v1/messages body into one
// segment per content block. A malformed body returns an error and no partial
// result.
func DecodeCompactionRequest(body []byte) (CompactionRequest, error) {
	var wire compactionWireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		// The caller reports no match on this path and reads no error, so this
		// is the only record that a body reached the decode and failed it.
		slog.Warn("adapter.anthropic.compaction_decode_failed",
			"concern", string(anthropicRequestLog),
			"component", "adapter",
			"body_bytes", len(body),
			"err", err,
		)
		return CompactionRequest{SessionID: "", Messages: nil},
			fmt.Errorf("decode compaction request: %w", err)
	}
	messages := make([]CompactionMessage, 0, len(wire.Messages))
	for _, message := range wire.Messages {
		parts, _ := NormalizeContent(message.Content)
		segments := make([]Segment, 0, len(parts))
		for _, part := range parts {
			segments = append(segments, segmentOfPart(part))
		}
		messages = append(messages, CompactionMessage{
			Role:     compactionRole(message.Role),
			Segments: segments,
		})
	}
	return CompactionRequest{
		SessionID: compactionSessionID(wire.Metadata),
		Messages:  messages,
	}, nil
}

func segmentOfPart(part content.Part) Segment {
	switch part.Kind {
	case content.PartText, content.PartRefusal:
		return Segment{Kind: SegmentText, Text: part.Text, ToolUseID: ""}
	case content.PartThinking:
		return Segment{Kind: SegmentThinking, Text: part.Text, ToolUseID: ""}
	case content.PartToolUse:
		return Segment{Kind: SegmentToolUse, Text: string(part.Input), ToolUseID: part.ID}
	case content.PartToolResult:
		return Segment{Kind: SegmentToolResult, Text: part.Text, ToolUseID: part.ToolUseID}
	case content.PartImage, content.PartAudio:
		return Segment{Kind: SegmentImage, Text: "", ToolUseID: ""}
	case content.PartUnsupported:
		return Segment{Kind: SegmentOther, Text: part.Text, ToolUseID: ""}
	}
	return Segment{Kind: SegmentOther, Text: part.Text, ToolUseID: ""}
}

func compactionSessionID(metadata *compactionMetadata) string {
	if metadata == nil || metadata.UserID == "" {
		return ""
	}
	var userID compactionUserID
	if err := json.Unmarshal([]byte(metadata.UserID), &userID); err != nil {
		return ""
	}
	return strings.TrimSpace(userID.SessionID)
}
