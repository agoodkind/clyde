package cursorjsonl

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Cursor wraps what the user typed in an envelope before storing it. A user
// turn arrives as a `<timestamp>` line naming when the turn happened followed
// by the typed text inside `<user_query>`, with any attachments the client
// gathered in sibling tags. The envelope is Cursor's framing rather than
// anything the user wrote or saw, so a reader that keeps it stores markup as
// conversation: measured on this machine, 10,378 of 10,432 user text parts
// carry a `<user_query>` wrapper and the framing accounts for 4.8% of user
// text bytes.
//
// The timestamp is worse than noise, because two otherwise identical turns
// differ only by the time inside them. Cursor's own injected prompts then read
// as distinct content: one prompt appeared 576 times across 45 transcripts as
// 58 distinct strings, so nothing downstream could see them as one message.
var (
	// userEnvelopeTimestampRe matches the leading timestamp line and the blank
	// line after it. It is anchored so a `<timestamp>` the user typed further
	// into their message stays where they put it.
	userEnvelopeTimestampRe = regexp.MustCompile(`\A\s*<timestamp>([^<]*)</timestamp>\s*`)
	// userEnvelopeQueryOpenRe and userEnvelopeQueryCloseRe match the markers
	// and the padding Cursor writes against them. Everything between and
	// around the markers is kept, so an attachment tag Cursor wrote beside the
	// query, and any text the user typed outside it, survive unchanged.
	//
	// The open pattern takes at most the one newline after the marker, never
	// the spaces after that: a paste beginning on an indented line keeps its
	// shape. The close pattern takes the whitespace before it, which is the
	// blank line Cursor pads with rather than anything the user can see.
	userEnvelopeQueryOpenRe  = regexp.MustCompile(`<user_query>\n?`)
	userEnvelopeQueryCloseRe = regexp.MustCompile(`(?s)\s*</user_query>`)
)

// userEnvelopeZoneRe splits Cursor's trailing zone off the timestamp value.
// Cursor writes the zone as a UTC offset in hours, "(UTC+2)" or "(UTC-7)",
// which Go's reference layouts cannot express, so the offset is read as a
// number and applied to the wall time parsed from the rest.
var userEnvelopeZoneRe = regexp.MustCompile(`\s*\(UTC([+-])(\d{1,2})(?::(\d{2}))?\)\s*\z`)

// userEnvelopeWallLayouts are the formats Cursor writes for the date and time,
// once the zone is removed. A value in none of them yields the zero time,
// which reads the same as a turn that carried no timestamp at all.
var userEnvelopeWallLayouts = []string{
	"Monday, Jan 2, 2006, 3:04 PM",
	"Monday, Jan 2, 2006, 3:04:05 PM",
	time.RFC3339,
}

// UnwrapUserEnvelope returns the text the user typed and the time Cursor
// recorded for the turn, given one user text part as stored.
//
// It removes the envelope's own markers and nothing else. Text that carries no
// envelope comes back unchanged with a zero time, which is what a transcript
// written before Cursor added the framing looks like.
func UnwrapUserEnvelope(text string) (string, time.Time) {
	stamped := time.Time{}
	remaining := text
	if match := userEnvelopeTimestampRe.FindStringSubmatch(remaining); match != nil {
		stamped = parseUserEnvelopeTimestamp(match[1])
		remaining = remaining[len(match[0]):]
	}
	// A query marker without its partner means the envelope is not the shape
	// this understands, so the text is left alone rather than half-unwrapped.
	// Deleting one marker from text whose other half is missing would edit
	// something the user may have typed themselves.
	opens := userEnvelopeQueryOpenRe.FindAllStringIndex(remaining, -1)
	closes := userEnvelopeQueryCloseRe.FindAllStringIndex(remaining, -1)
	if len(opens) == 1 && len(closes) == 1 && opens[0][0] < closes[0][0] {
		// The two matches are already located, so the markers come out by
		// slicing around them. Replacing instead would scan the whole message
		// twice more to find the same positions, on a path that runs for
		// nearly every user turn in every conversation.
		open, closed := opens[0], closes[0]
		remaining = remaining[:open[0]] + remaining[open[1]:closed[0]] + remaining[closed[1]:]
	}
	// What is left is the user's, so it is returned as it stands. The patterns
	// above consume the envelope's own newlines, and trimming further would
	// take the leading indentation off a pasted code block: the composer store
	// learned that in TestMapComposerBubbleIndentedCodeAfterEnvelopeSurvives.
	// Text carrying no envelope is likewise byte-identical, which
	// TestMapJSONLMessageUserRoleNeverStripped requires.
	return remaining, stamped
}

// parseUserEnvelopeTimestamp reads one `<timestamp>` value, returning the zero
// time for a value written in a layout this does not know. An unparsed stamp
// costs the turn its time and nothing else, because the value is removed from
// the text either way: leaving it in would keep two identical turns looking
// different, which is the reason for removing it.
func parseUserEnvelopeTimestamp(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}
	}
	zone := time.UTC
	if match := userEnvelopeZoneRe.FindStringSubmatch(trimmed); match != nil {
		zone = userEnvelopeZone(match[1], match[2], match[3])
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(match[0])])
	}
	for _, layout := range userEnvelopeWallLayouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, zone); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// userEnvelopeZone builds the location for one parsed "(UTC+2)" suffix. The
// sign and hour always appear; the minutes appear only in zones that need
// them, so an absent minutes group counts as zero.
//
// Both numbers come from digit-only capture groups in [userEnvelopeZoneRe],
// so neither conversion can fail and neither error is checked.
func userEnvelopeZone(sign, hours, minutes string) *time.Location {
	offsetHours, _ := strconv.Atoi(hours)
	offsetMinutes := 0
	if minutes != "" {
		offsetMinutes, _ = strconv.Atoi(minutes)
	}
	seconds := offsetHours*3600 + offsetMinutes*60
	if sign == "-" {
		seconds = -seconds
	}
	return time.FixedZone("UTC"+sign+hours, seconds)
}
