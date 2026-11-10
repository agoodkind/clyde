package compaction

import (
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/reorientinject"
)

const (
	logConcern   = "providers.mitm.wire"
	logComponent = "mitm"
)

// Provider adapts the Anthropic Messages wire protocol to the compaction split.
// It owns every Anthropic shape the split needs, and the split names none of
// them.
type Provider struct{}

// NewProvider returns the Claude compaction provider.
func NewProvider() Provider { return Provider{} }

// ParseCompaction decodes an intercepted /v1/messages body. It returns
// ok=false for a body that is not a compaction request, which leaves the
// request alone.
func (Provider) ParseCompaction(body []byte) (reorientinject.ParsedRequest, bool) {
	decoded, err := anthropic.DecodeCompactionRequest(body)
	if err != nil {
		return noRequest(), false
	}
	roles := make([]string, 0, len(decoded.Messages))
	texts := make([]string, 0, len(decoded.Messages))
	messages := make([]reorientinject.Message, 0, len(decoded.Messages))
	for _, message := range decoded.Messages {
		roles = append(roles, string(message.Role))
		texts = append(texts, messageText(message))
		messages = append(messages, reorientinject.Message{
			Role:     role(message.Role),
			Segments: segments(message),
		})
	}
	promptIndex, ok := PromptIndex(roles, texts)
	if !ok {
		return noRequest(), false
	}
	return reorientinject.ParsedRequest{
		SessionID:        decoded.SessionID,
		Messages:         messages,
		RetainStart:      0,
		InstructionStart: promptIndex,
		Arguments:        Arguments(texts[promptIndex]),
	}, true
}

// Truncate rewrites body to end the conversation at the cut.
func (Provider) Truncate(
	body []byte,
	cut reorientinject.Cut,
	instructionStart int,
) ([]byte, error) {
	truncated, err := anthropic.TruncateCompactionRequest(body, anthropic.CompactionCut{
		MessageIndex: cut.MessageIndex,
		SegmentIndex: cut.SegmentIndex,
		HeadRunes:    cut.HeadRunes,
	}, instructionStart)
	if err != nil {
		slog.Warn("providers.claude.compaction_truncate_failed",
			"concern", logConcern,
			"component", logComponent,
			"message_index", cut.MessageIndex,
			"segment_index", cut.SegmentIndex,
			"head_runes", cut.HeadRunes,
			"err", err,
		)
		return nil, fmt.Errorf("truncate claude compaction request: %w", err)
	}
	return truncated, nil
}

// InjectSummary inserts the wrapped injection into the summary the model
// returned.
func (Provider) InjectSummary(body []byte, injection string) ([]byte, error) {
	injected, err := anthropic.InjectIntoSummary(body, injection)
	if err != nil {
		slog.Warn("providers.claude.compaction_inject_failed",
			"concern", logConcern,
			"component", logComponent,
			"injection_bytes", len(injection),
			"err", err,
		)
		return nil, fmt.Errorf("inject into claude summary: %w", err)
	}
	return injected, nil
}

// noRequest is the zero result ParseCompaction returns when the body is not a
// compaction request.
func noRequest() reorientinject.ParsedRequest {
	return reorientinject.ParsedRequest{
		SessionID:        "",
		Messages:         nil,
		RetainStart:      0,
		InstructionStart: 0,
		Arguments:        nil,
	}
}

// messageText concatenates a message's text blocks. PromptIndex and Arguments
// read it; the token count reads the segments instead.
func messageText(message anthropic.CompactionMessage) string {
	parts := make([]string, 0, len(message.Segments))
	for _, segment := range message.Segments {
		if segment.Kind != anthropic.SegmentText || segment.Text == "" {
			continue
		}
		parts = append(parts, segment.Text)
	}
	return strings.Join(parts, "\n")
}

func role(wire anthropic.CompactionRole) reorientinject.Role {
	switch wire {
	case anthropic.CompactionRoleUser:
		return reorientinject.RoleUser
	case anthropic.CompactionRoleAssistant:
		return reorientinject.RoleAssistant
	case anthropic.CompactionRoleSystem:
		return reorientinject.RoleSystem
	case anthropic.CompactionRoleOther:
		return reorientinject.RoleOther
	}
	return reorientinject.RoleOther
}

func segments(message anthropic.CompactionMessage) []reorientinject.Segment {
	out := make([]reorientinject.Segment, 0, len(message.Segments))
	for _, segment := range message.Segments {
		out = append(out, reorientinject.Segment{
			Kind:   segmentKind(segment.Kind),
			Text:   segment.Text,
			Atomic: false,
		})
	}
	return out
}

func segmentKind(kind anthropic.SegmentKind) reorientinject.SegmentKind {
	switch kind {
	case anthropic.SegmentText:
		return reorientinject.KindText
	case anthropic.SegmentThinking:
		return reorientinject.KindThinking
	case anthropic.SegmentToolUse:
		return reorientinject.KindToolUse
	case anthropic.SegmentToolResult:
		return reorientinject.KindToolResult
	case anthropic.SegmentImage:
		return reorientinject.KindImage
	case anthropic.SegmentOther:
		return reorientinject.KindOther
	}
	return reorientinject.KindOther
}
