package reorientinject

import (
	"slices"
	"strings"

	"goodkind.io/clyde/internal/reorienttag"
)

const (
	unframedHeadingPrefix = "### "
	unframedSeparator     = "\n\n"
	unframedOtherHeading  = "Other"
)

func unframedHeading(role Role) string {
	switch role {
	case RoleUser:
		return "User"
	case RoleAssistant:
		return "Assistant"
	case RoleSystem:
		return "System"
	case RoleOther:
		return unframedOtherHeading
	}
	return unframedOtherHeading
}

// unframedCandidate stores a first retained segment and its retained tail
// text.
type unframedCandidate struct {
	message int
	segment int
	text    string
}

// ReconstructUnframedTranscript tests each retained segment as the start of
// unframed text. It returns typed segments only when exactly one
// reconstruction matches.
func ReconstructUnframedTranscript(request ParsedRequest, text string) ([]reorienttag.Segment, bool) {
	var found []reorienttag.Segment
	matchCount := 0
	for messageIndex := request.RetainStart; messageIndex < request.InstructionStart; messageIndex++ {
		for segmentIndex, segment := range request.Messages[messageIndex].Segments {
			for _, tail := range unframedTails(request, messageIndex, segmentIndex, segment, text) {
				candidate := unframedCandidate{message: messageIndex, segment: segmentIndex, text: tail}
				segments := unframedSegments(request, candidate)
				if matchCount > 0 && slices.Equal(found, segments) {
					continue
				}
				found = segments
				matchCount++
			}
		}
	}
	if matchCount != 1 {
		return nil, false
	}
	return found, true
}

// unframedTails returns the retained tail when text starts with the message
// heading and ends with the rendered following segments. It requires an atomic
// first segment to match its whole text because the splitter never retains
// part of an atomic segment.
func unframedTails(request ParsedRequest, messageIndex int, segmentIndex int, segment Segment, text string) []string {
	heading := unframedHeadingPrefix + unframedHeading(request.Messages[messageIndex].Role) + unframedSeparator
	body, hasHeading := strings.CutPrefix(text, heading)
	if !hasHeading {
		return nil
	}
	following := renderUnframed(request, unframedCandidate{message: messageIndex, segment: segmentIndex + 1, text: ""}, true)
	if !strings.HasSuffix(body, following) {
		return nil
	}
	tail := strings.TrimSuffix(body, following)
	if strings.TrimSpace(tail) == "" || !strings.HasSuffix(segment.Text, tail) {
		return nil
	}
	if segment.Atomic && strings.TrimSpace(tail) != strings.TrimSpace(segment.Text) {
		return nil
	}
	return []string{tail}
}

// renderUnframed renders the retained segments after start in the unframed
// format.
func renderUnframed(request ParsedRequest, start unframedCandidate, headingWritten bool) string {
	var builder strings.Builder
	for messageIndex := start.message; messageIndex < request.InstructionStart; messageIndex++ {
		message := request.Messages[messageIndex]
		written := 0
		if messageIndex == start.message && headingWritten {
			written = 1
		}
		for segmentIndex, segment := range message.Segments {
			if messageIndex == start.message && segmentIndex < start.segment {
				continue
			}
			if strings.TrimSpace(segment.Text) == "" {
				continue
			}
			if written == 0 {
				builder.WriteString(unframedSeparator + unframedHeadingPrefix + unframedHeading(message.Role))
			}
			builder.WriteString(unframedSeparator)
			builder.WriteString(segment.Text)
			written++
		}
	}
	return strings.TrimRight(builder.String(), " \t\r\n")
}

func unframedSegments(request ParsedRequest, start unframedCandidate) []reorienttag.Segment {
	segments := make([]reorienttag.Segment, 0)
	for messageIndex := start.message; messageIndex < request.InstructionStart; messageIndex++ {
		message := request.Messages[messageIndex]
		for segmentIndex, segment := range message.Segments {
			if messageIndex == start.message && segmentIndex < start.segment {
				continue
			}
			text := segment.Text
			if messageIndex == start.message && segmentIndex == start.segment {
				text = start.text
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			segments = append(segments, frameSegment(message.Role, segment, text))
		}
	}
	return segments
}
