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
	// alone. Only the markers are removed and everything between and around
	// them is kept, so an attachment tag Cursor wrote beside the query, and any
	// text the user typed outside it, survive unchanged.
	userEnvelopeQueryOpenRe  = regexp.MustCompile(`(?s)<user_query>\n?`)
	userEnvelopeQueryCloseRe = regexp.MustCompile(`(?s)\n?</user_query>`)
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
	unwrapped := false
	if match := userEnvelopeTimestampRe.FindStringSubmatch(remaining); match != nil {
		stamped = parseUserEnvelopeTimestamp(match[1])
		remaining = remaining[len(match[0]):]
		unwrapped = true
	}
	// A query marker without its partner means the envelope is not the shape
	// this understands, so the text is left alone rather than half-unwrapped.
	// Deleting one marker from text whose other half is missing would edit
	// something the user may have typed themselves.
	opens := userEnvelopeQueryOpenRe.FindAllStringIndex(remaining, -1)
	closes := userEnvelopeQueryCloseRe.FindAllStringIndex(remaining, -1)
	if len(opens) == 1 && len(closes) == 1 && opens[0][0] < closes[0][0] {
		remaining = userEnvelopeQueryCloseRe.ReplaceAllString(remaining, "")
		remaining = userEnvelopeQueryOpenRe.ReplaceAllString(remaining, "")
		unwrapped = true
	}
	// Text carrying no envelope comes back byte-identical. Trimming it would
	// edit a message Cursor stored exactly as the user typed it, and a user
	// who pastes marker syntax into their own message must keep every byte:
	// see TestMapJSONLMessageUserRoleNeverStripped.
	if !unwrapped {
		return text, stamped
	}
	return strings.TrimSpace(remaining), stamped
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
func userEnvelopeZone(sign, hours, minutes string) *time.Location {
	offsetHours, err := strconv.Atoi(hours)
	if err != nil {
		return time.UTC
	}
	offsetMinutes := 0
	if minutes != "" {
		parsedMinutes, minutesErr := strconv.Atoi(minutes)
		if minutesErr != nil {
			return time.UTC
		}
		offsetMinutes = parsedMinutes
	}
	seconds := offsetHours*3600 + offsetMinutes*60
	if sign == "-" {
		seconds = -seconds
	}
	return time.FixedZone("UTC"+sign+hours, seconds)
}
