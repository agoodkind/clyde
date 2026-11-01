package reorienttag

import (
	"strconv"
	"strings"
)

// TranscriptFormatMarker marks length framed segments when it follows the
// transcript open tag on its own line. A span without the marker uses unframed
// text and does not record segment kinds.
const TranscriptFormatMarker = `<clyde-transcript-format version="2"/>`

// SegmentKind identifies the content kind of a framed segment.
type SegmentKind string

const (
	// SegmentKindChat identifies user or assistant text.
	SegmentKindChat SegmentKind = "chat"
	// SegmentKindThinking identifies model reasoning text.
	SegmentKindThinking SegmentKind = "thinking"
	// SegmentKindToolCall identifies tool call input.
	SegmentKindToolCall SegmentKind = "tool_call"
	// SegmentKindToolOutput identifies tool call result text.
	SegmentKindToolOutput SegmentKind = "tool_output"
	// SegmentKindSystem identifies text from a system role message, including
	// hook context sent as a system message.
	SegmentKindSystem SegmentKind = "system"
	// SegmentKindOther identifies a block kind or role that the injector does
	// not classify.
	SegmentKindOther SegmentKind = "other"
)

// SegmentRole identifies the wire role of the message containing a framed
// segment.
type SegmentRole string

const (
	// SegmentRoleUser identifies a user message.
	SegmentRoleUser SegmentRole = "user"
	// SegmentRoleAssistant identifies an assistant message.
	SegmentRoleAssistant SegmentRole = "assistant"
	// SegmentRoleSystem identifies a system message.
	SegmentRoleSystem SegmentRole = "system"
	// SegmentRoleOther identifies a role that the injector does not name.
	SegmentRoleOther SegmentRole = "other"
)

// Segment stores a retained content block with its role, kind, and exact text.
type Segment struct {
	Role SegmentRole
	Kind SegmentKind
	Text string
}

// TranscriptSpan stores a framed transcript and its decoded segments in order.
// Start is the byte offset of the open tag, and End is the byte offset after
// the close tag.
type TranscriptSpan struct {
	Start    int
	End      int
	Segments []Segment
}

const (
	segmentRolePrefix  = "<segment role="
	segmentKindPrefix  = " kind="
	segmentBytesPrefix = " bytes="
	segmentHeaderEnd   = ">\n"
	segmentFooter      = "\n</segment>"
	segmentSeparator   = "\n\n"
)

// EncodeSegment writes a header with Go quoted role, kind, and byte count
// attributes, followed by a newline, the text, another newline, and </segment>.
// The byte count lets a parser read text containing any characters, including
// frame and transcript tags.
func EncodeSegment(segment Segment) string {
	var builder strings.Builder
	builder.WriteString(segmentRolePrefix)
	builder.WriteString(strconv.Quote(string(segment.Role)))
	builder.WriteString(segmentKindPrefix)
	builder.WriteString(strconv.Quote(string(segment.Kind)))
	builder.WriteString(segmentBytesPrefix)
	builder.WriteString(strconv.Quote(strconv.Itoa(len(segment.Text))))
	builder.WriteString(segmentHeaderEnd)
	builder.WriteString(segment.Text)
	builder.WriteString(segmentFooter)
	return builder.String()
}

// EncodeTranscript returns the marker line and encoded segments, with a blank
// line between segments. It returns an empty string for zero segments.
func EncodeTranscript(segments []Segment) string {
	if len(segments) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(TranscriptFormatMarker)
	builder.WriteByte('\n')
	for index, segment := range segments {
		if index > 0 {
			builder.WriteString(segmentSeparator)
		}
		builder.WriteString(EncodeSegment(segment))
	}
	return builder.String()
}

// FramedTranscript returns the EncodeTranscript output between the transcript
// tags, or an empty string for zero segments.
func FramedTranscript(segments []Segment) string {
	if len(segments) == 0 {
		return ""
	}
	return PreCompactionTranscriptOpen + "\n" + EncodeTranscript(segments) + "\n" + PreCompactionTranscriptClose
}

// ParseTranscriptSpans returns valid framed transcripts in text order. It reads
// segment text by byte count and requires exact frame footers, blank lines
// between segments, and a newline before the close tag. It skips invalid
// candidates and unframed spans.
func ParseTranscriptSpans(text string) []TranscriptSpan {
	header := PreCompactionTranscriptOpen + "\n" + TranscriptFormatMarker + "\n"
	var spans []TranscriptSpan
	position := 0
	for position < len(text) {
		offset := strings.Index(text[position:], header)
		if offset < 0 {
			break
		}
		start := position + offset
		segments, end, ok := parseFramedBody(text, start+len(header))
		if !ok {
			position = start + len(header)
			continue
		}
		spans = append(spans, TranscriptSpan{Start: start, End: end, Segments: segments})
		position = end
	}
	return spans
}

// RemoveTranscriptSpans removes the framed transcripts found by
// ParseTranscriptSpans and reports whether it removed any.
func RemoveTranscriptSpans(text string) (string, bool) {
	spans := ParseTranscriptSpans(text)
	if len(spans) == 0 {
		return text, false
	}
	return textOutsideSpans(text, spans), true
}

// CompleteTranscriptSpans returns the ParseTranscriptSpans result when those
// spans contain every transcript wrapper start in text. It returns nil when a
// wrapper start remains outside those spans because a framed transcript can be
// a quotation inside an unframed wrapper.
func CompleteTranscriptSpans(text string) []TranscriptSpan {
	spans := ParseTranscriptSpans(text)
	if ContainsTranscriptWrapper(textOutsideSpans(text, spans)) {
		return nil
	}
	return spans
}

func textOutsideSpans(text string, spans []TranscriptSpan) string {
	var builder strings.Builder
	previousEnd := 0
	for _, span := range spans {
		builder.WriteString(text[previousEnd:span.Start])
		previousEnd = span.End
	}
	builder.WriteString(text[previousEnd:])
	return builder.String()
}

// ContainsTranscriptWrapper reports whether text contains the start of a Clyde
// transcript wrapper. A start has an open tag and newline, then a format marker
// line or an unframed role heading line of ### User, ### Assistant, ### System,
// or ### Other followed by a blank line.
func ContainsTranscriptWrapper(text string) bool {
	if strings.Contains(text, PreCompactionTranscriptOpen+"\n"+TranscriptFormatMarker+"\n") {
		return true
	}
	for _, heading := range [...]string{"User", "Assistant", "System", "Other"} {
		if strings.Contains(text, PreCompactionTranscriptOpen+"\n### "+heading+"\n\n") {
			return true
		}
	}
	return false
}

func parseFramedBody(text string, position int) ([]Segment, int, bool) {
	closing := "\n" + PreCompactionTranscriptClose
	var segments []Segment
	for {
		segment, next, ok := parseSegment(text, position)
		if !ok {
			return nil, 0, false
		}
		segments = append(segments, segment)
		position = next
		if strings.HasPrefix(text[position:], closing) {
			return segments, position + len(closing), true
		}
		if !strings.HasPrefix(text[position:], segmentSeparator) {
			return nil, 0, false
		}
		position += len(segmentSeparator)
	}
}

func parseSegment(text string, position int) (Segment, int, bool) {
	noSegment := Segment{Role: "", Kind: "", Text: ""}
	position, ok := consumeLiteral(text, position, segmentRolePrefix)
	if !ok {
		return noSegment, 0, false
	}
	role, position, ok := consumeQuoted(text, position)
	if !ok || !knownRole(SegmentRole(role)) {
		return noSegment, 0, false
	}
	position, ok = consumeLiteral(text, position, segmentKindPrefix)
	if !ok {
		return noSegment, 0, false
	}
	kind, position, ok := consumeQuoted(text, position)
	if !ok || !knownKind(SegmentKind(kind)) {
		return noSegment, 0, false
	}
	position, ok = consumeLiteral(text, position, segmentBytesPrefix)
	if !ok {
		return noSegment, 0, false
	}
	length, position, ok := consumeLength(text, position)
	if !ok {
		return noSegment, 0, false
	}
	position, ok = consumeLiteral(text, position, segmentHeaderEnd)
	if !ok || length > len(text)-position {
		return noSegment, 0, false
	}
	body := text[position : position+length]
	position, ok = consumeLiteral(text, position+length, segmentFooter)
	if !ok {
		return noSegment, 0, false
	}
	return Segment{Role: SegmentRole(role), Kind: SegmentKind(kind), Text: body}, position, true
}

func consumeLiteral(text string, position int, literal string) (int, bool) {
	if !strings.HasPrefix(text[position:], literal) {
		return 0, false
	}
	return position + len(literal), true
}

func consumeQuoted(text string, position int) (string, int, bool) {
	quoted, err := strconv.QuotedPrefix(text[position:])
	if err != nil {
		return "", 0, false
	}
	value, err := strconv.Unquote(quoted)
	if err != nil {
		return "", 0, false
	}
	return value, position + len(quoted), true
}

func consumeLength(text string, position int) (int, int, bool) {
	value, next, ok := consumeQuoted(text, position)
	if !ok || value == "" || value != strings.TrimLeft(value, "+-") {
		return 0, 0, false
	}
	length, err := strconv.Atoi(value)
	if err != nil || length < 0 || strconv.Itoa(length) != value {
		return 0, 0, false
	}
	return length, next, true
}

func knownRole(role SegmentRole) bool {
	switch role {
	case SegmentRoleUser, SegmentRoleAssistant, SegmentRoleSystem, SegmentRoleOther:
		return true
	}
	return false
}

func knownKind(kind SegmentKind) bool {
	switch kind {
	case SegmentKindChat,
		SegmentKindThinking,
		SegmentKindToolCall,
		SegmentKindToolOutput,
		SegmentKindSystem,
		SegmentKindOther:
		return true
	}
	return false
}
