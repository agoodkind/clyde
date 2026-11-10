// Package compaction adapts the Codex Responses wire protocol to the shared
// compaction split. It owns every Responses shape the split needs, and the
// split names none of them.
package compaction

import (
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/reorientinject"
)

const (
	logConcern   = "adapter.providers.codex.request"
	logComponent = "adapter"
)

// Provider implements reorientinject.Provider for a native Codex compaction
// request. The adapter gates detection on the turn metadata header before it
// runs the splitter; this provider reads only the request body.
type Provider struct{}

// NewProvider returns the Codex compaction provider.
func NewProvider() Provider { return Provider{} }

// NewSplitter returns the generic splitter, built on this provider, in the
// shape the Codex adapter consumes. The daemon passes it as
// adapter.Deps.RawResponsesCompaction.
func NewSplitter(settings reorientinject.Settings) codex.CompactionSplitter {
	return splitter{inner: reorientinject.NewSplitter(NewProvider(), settings)}
}

// splitter adapts reorientinject.Result to codex.CompactionSplit.
type splitter struct {
	inner *reorientinject.Splitter
}

func (s splitter) Plan(body []byte) (codex.CompactionSplit, bool) {
	result, ok := s.inner.Plan(body)
	if !ok {
		return codex.CompactionSplit{Forwarded: nil, Injection: ""}, false
	}
	return codex.CompactionSplit{Forwarded: result.Forwarded, Injection: result.Injection}, true
}

// ParseCompaction decodes the Responses input array. It returns ok=false for
// a body the split must forward unchanged.
func (Provider) ParseCompaction(body []byte) (reorientinject.ParsedRequest, bool) {
	decoded, ok := codex.DecodeCompactionInput(body)
	if !ok {
		return noRequest(), false
	}
	messages := make([]reorientinject.Message, 0, len(decoded.Messages))
	for _, message := range decoded.Messages {
		messages = append(messages, reorientinject.Message{
			Role:     role(message.Role),
			Segments: segments(message.Segments),
		})
	}
	// The Codex compaction prompt is a fixed client string with no argument
	// block, and the adapter reads the session id from the turn metadata
	// header for the registry.
	return reorientinject.ParsedRequest{
		SessionID:        "",
		Messages:         messages,
		RetainStart:      decoded.RetainStart,
		InstructionStart: decoded.InstructionStart,
		Arguments:        nil,
	}, true
}

// Truncate rewrites body to end the counted conversation at the cut.
func (Provider) Truncate(
	body []byte,
	cut reorientinject.Cut,
	instructionStart int,
) ([]byte, error) {
	decoded, ok := codex.DecodeCompactionInput(body)
	if !ok {
		return nil, errNotCompaction
	}
	truncated, err := codex.TruncateCompactionInput(body, codex.CompactionCut{
		MessageIndex: cut.MessageIndex,
		SegmentIndex: cut.SegmentIndex,
		HeadRunes:    cut.HeadRunes,
	}, decoded.RetainStart, instructionStart)
	if err != nil {
		slog.Warn("providers.codex.compaction_truncate_failed",
			"concern", logConcern,
			"component", logComponent,
			"message_index", cut.MessageIndex,
			"segment_index", cut.SegmentIndex,
			"head_runes", cut.HeadRunes,
			"err", err,
		)
		return nil, fmt.Errorf("truncate codex compaction request: %w", err)
	}
	return truncated, nil
}

// InjectSummary returns body unchanged. On the native Codex path the adapter
// response transformer appends the injection to the summary, and the
// splitter's Inject method has no Codex caller.
func (Provider) InjectSummary(body []byte, _ string) ([]byte, error) {
	return body, nil
}

var errNotCompaction = fmt.Errorf("body is not a codex compaction request")

func noRequest() reorientinject.ParsedRequest {
	return reorientinject.ParsedRequest{
		SessionID:        "",
		Messages:         nil,
		RetainStart:      0,
		InstructionStart: 0,
		Arguments:        nil,
	}
}

func role(wire codex.CompactionRole) reorientinject.Role {
	switch wire {
	case codex.CompactionRoleUser:
		return reorientinject.RoleUser
	case codex.CompactionRoleAssistant:
		return reorientinject.RoleAssistant
	case codex.CompactionRoleDeveloper:
		return reorientinject.RoleSystem
	}
	return reorientinject.RoleOther
}

func segments(wire []codex.CompactionSegment) []reorientinject.Segment {
	out := make([]reorientinject.Segment, 0, len(wire))
	for _, segment := range wire {
		out = append(out, reorientinject.Segment{
			Kind:   segmentKind(segment.Kind),
			Text:   segment.Text,
			Atomic: segment.Atomic,
		})
	}
	return out
}

func segmentKind(kind codex.CompactionSegmentKind) reorientinject.SegmentKind {
	switch kind {
	case codex.CompactionSegmentText:
		return reorientinject.KindText
	case codex.CompactionSegmentThinking:
		return reorientinject.KindThinking
	case codex.CompactionSegmentToolUse:
		return reorientinject.KindToolUse
	case codex.CompactionSegmentToolResult:
		return reorientinject.KindToolResult
	case codex.CompactionSegmentImage:
		return reorientinject.KindImage
	case codex.CompactionSegmentOther:
		return reorientinject.KindOther
	}
	return reorientinject.KindOther
}
