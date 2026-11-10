package parser

import (
	"sort"
	"strings"
	"unicode"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	// Unframed transcript text starts each message with the heading prefix and
	// role name. A blank line separates segments.
	legacyHeadingPrefix    = "### "
	legacySegmentSeparator = "\n\n"
	legacySummaryClose     = "</summary>"
	legacyEscapedClose     = `<\/summary>`
	// Claude Code converts CRLF to LF and reduces runs of more than two line feeds
	// to two when storing summary text.
	legacyMaximumLineFeeds = 2
)

// legacyCut records the first retained segment and the number of its leading
// runes that the model summarized.
type legacyCut struct {
	message   int
	segment   int
	headRunes int
}

// renderPiece records a byte range of rendered source text for a heading,
// separator, or segment. For a segment, it stores the message index, segment
// index, role, kind, full text, and byte length of the removed head.
type renderPiece struct {
	sourceStart int
	sourceEnd   int
	framing     bool
	message     int
	segment     int
	role        reorienttag.SegmentRole
	kind        reorienttag.SegmentKind
	fullText    string
	headBytes   int
}

// legacyRendering stores unframed transcript text before storage normalization
// and the pieces that cover it.
type legacyRendering struct {
	source string
	pieces []renderPiece
}

// persistedRendering stores transcript text after summary close escaping and
// line feed normalization, with the source byte offset of each stored byte.
type persistedRendering struct {
	text      string
	sourceMap []int32
}

// renderLegacy renders messages from the cut through the message before end
// with one role heading per message with retained text and blank lines between
// segments. It retains only the cut segment's tail, skips blank segments, and
// trims surrounding whitespace.
func renderLegacy(request anthropic.CompactionRequest, cut legacyCut, end int) legacyRendering {
	var builder strings.Builder
	pieces := make([]renderPiece, 0)
	for messageIndex := cut.message; messageIndex < end; messageIndex++ {
		message := request.Messages[messageIndex]
		written := 0
		for segmentIndex, segment := range message.Segments {
			if messageIndex == cut.message && segmentIndex < cut.segment {
				continue
			}
			text := segment.Text
			headBytes := 0
			if messageIndex == cut.message && segmentIndex == cut.segment {
				headBytes = runeOffset(text, cut.headRunes)
				text = text[headBytes:]
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			framing := legacySegmentSeparator
			if written == 0 {
				framing = legacySegmentSeparator + legacyHeadingPrefix + legacyHeading(message.Role) + legacySegmentSeparator
			}
			pieces = append(pieces, framingPiece(builder.Len(), framing))
			builder.WriteString(framing)
			pieces = append(pieces, renderPiece{
				sourceStart: builder.Len(),
				sourceEnd:   builder.Len() + len(text),
				framing:     false,
				message:     messageIndex,
				segment:     segmentIndex,
				role:        legacySegmentRole(message.Role),
				kind:        legacySegmentKind(message.Role, segment.Kind),
				fullText:    segment.Text,
				headBytes:   headBytes,
			})
			builder.WriteString(text)
			written++
		}
	}
	return trimRendering(builder.String(), pieces)
}

func framingPiece(start int, text string) renderPiece {
	return renderPiece{
		sourceStart: start,
		sourceEnd:   start + len(text),
		framing:     true,
		message:     -1,
		segment:     -1,
		role:        "",
		kind:        "",
		fullText:    "",
		headBytes:   0,
	}
}

// trimRendering trims leading and trailing whitespace from the source and
// clips each piece to the trimmed range.
func trimRendering(source string, pieces []renderPiece) legacyRendering {
	low := len(source) - len(strings.TrimLeftFunc(source, unicode.IsSpace))
	high := max(len(strings.TrimRightFunc(source, unicode.IsSpace)), low)
	trimmed := make([]renderPiece, 0, len(pieces))
	for _, piece := range pieces {
		piece.sourceStart = min(max(piece.sourceStart, low), high) - low
		piece.sourceEnd = min(max(piece.sourceEnd, low), high) - low
		if piece.sourceEnd <= piece.sourceStart {
			continue
		}
		trimmed = append(trimmed, piece)
	}
	return legacyRendering{source: source[low:high], pieces: trimmed}
}

// runeOffset returns the byte offset after the requested number of leading
// runes in text.
func runeOffset(text string, runes int) int {
	if runes <= 0 {
		return 0
	}
	count := 0
	for offset := range text {
		if count == runes {
			return offset
		}
		count++
	}
	return len(text)
}

func legacyHeading(role anthropic.CompactionRole) string {
	switch role {
	case anthropic.CompactionRoleUser:
		return "User"
	case anthropic.CompactionRoleAssistant:
		return "Assistant"
	case anthropic.CompactionRoleSystem:
		return "System"
	case anthropic.CompactionRoleOther:
		return "Other"
	}
	return "Other"
}

func legacySegmentRole(role anthropic.CompactionRole) reorienttag.SegmentRole {
	switch role {
	case anthropic.CompactionRoleUser:
		return reorienttag.SegmentRoleUser
	case anthropic.CompactionRoleAssistant:
		return reorienttag.SegmentRoleAssistant
	case anthropic.CompactionRoleSystem:
		return reorienttag.SegmentRoleSystem
	case anthropic.CompactionRoleOther:
		return reorienttag.SegmentRoleOther
	}
	return reorienttag.SegmentRoleOther
}

// legacySegmentKind maps user and assistant text to chat, system text to
// system, thinking to thinking, tool use to tool_call, tool result to
// tool_output, and other blocks to other.
func legacySegmentKind(role anthropic.CompactionRole, kind anthropic.SegmentKind) reorienttag.SegmentKind {
	switch kind {
	case anthropic.SegmentText:
		return legacyTextKind(role)
	case anthropic.SegmentThinking:
		return reorienttag.SegmentKindThinking
	case anthropic.SegmentToolUse:
		return reorienttag.SegmentKindToolCall
	case anthropic.SegmentToolResult:
		return reorienttag.SegmentKindToolOutput
	case anthropic.SegmentImage, anthropic.SegmentOther:
		return reorienttag.SegmentKindOther
	}
	return reorienttag.SegmentKindOther
}

func legacyTextKind(role anthropic.CompactionRole) reorienttag.SegmentKind {
	switch role {
	case anthropic.CompactionRoleUser, anthropic.CompactionRoleAssistant:
		return reorienttag.SegmentKindChat
	case anthropic.CompactionRoleSystem:
		return reorienttag.SegmentKindSystem
	case anthropic.CompactionRoleOther:
		return reorienttag.SegmentKindOther
	}
	return reorienttag.SegmentKindOther
}

// persistLegacy escapes each summary close tag, converts CRLF to LF, then
// reduces line feed runs to two. It records the source offset of every output
// byte.
func persistLegacy(source string) persistedRendering {
	escaped, escapedMap := escapeSummaryClose(source)
	unixText, unixMap := dropCarriageReturns(escaped, escapedMap)
	return collapseLineFeeds(unixText, unixMap)
}

func escapeSummaryClose(source string) ([]byte, []int32) {
	out := make([]byte, 0, len(source))
	sourceMap := make([]int32, 0, len(source))
	for index := 0; index < len(source); index++ {
		if strings.HasPrefix(source[index:], legacySummaryClose) {
			out = append(out, legacyEscapedClose[0])
			sourceMap = append(sourceMap, int32(index))
			for offset := 1; offset < len(legacyEscapedClose); offset++ {
				out = append(out, legacyEscapedClose[offset])
				sourceMap = append(sourceMap, int32(index+max(offset-1, 1)))
			}
			index += len(legacySummaryClose) - 1
			continue
		}
		out = append(out, source[index])
		sourceMap = append(sourceMap, int32(index))
	}
	return out, sourceMap
}

func dropCarriageReturns(text []byte, sourceMap []int32) ([]byte, []int32) {
	out := make([]byte, 0, len(text))
	outMap := make([]int32, 0, len(text))
	for index, character := range text {
		if character == '\r' && index+1 < len(text) && text[index+1] == '\n' {
			continue
		}
		out = append(out, character)
		outMap = append(outMap, sourceMap[index])
	}
	return out, outMap
}

func collapseLineFeeds(text []byte, sourceMap []int32) persistedRendering {
	out := make([]byte, 0, len(text))
	outMap := make([]int32, 0, len(text))
	run := 0
	for index, character := range text {
		if character == '\n' {
			run++
			if run > legacyMaximumLineFeeds {
				continue
			}
		} else {
			run = 0
		}
		out = append(out, character)
		outMap = append(outMap, sourceMap[index])
	}
	return persistedRendering{text: string(out), sourceMap: outMap}
}

// outputOffset returns the first stored byte that comes from source offset
// sourceOffset or later.
func (rendering persistedRendering) outputOffset(sourceOffset int) int {
	return sort.Search(len(rendering.sourceMap), func(index int) bool {
		return int(rendering.sourceMap[index]) >= sourceOffset
	})
}
