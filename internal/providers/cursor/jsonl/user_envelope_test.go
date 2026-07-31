package cursorjsonl

import (
	"testing"
	"time"
)

// TestUnwrapUserEnvelopeKeepsWhatTheUserTyped covers the shapes Cursor writes,
// using text taken from real transcripts.
func TestUnwrapUserEnvelopeKeepsWhatTheUserTyped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		stored  string
		want    string
		wantUTC string
	}{
		{
			name:    "timestamp and query",
			stored:  "<timestamp>Wednesday, Jul 29, 2026, 10:27 PM (UTC+2)</timestamp>\n\n<user_query>Briefly inform the user about the task result.</user_query>",
			want:    "Briefly inform the user about the task result.",
			wantUTC: "2026-07-29T20:27:00Z",
		},
		{
			name:   "query with no timestamp",
			stored: "<user_query>\n@AGENTS.md:47\n\nCan you verify this against the real code?\n\n</user_query>",
			want:   "@AGENTS.md:47\n\nCan you verify this against the real code?",
		},
		{
			name:   "attachment beside the query survives",
			stored: "<user_query>fix the build</user_query>\n<attached_files>main.go</attached_files>",
			want:   "fix the build\n<attached_files>main.go</attached_files>",
		},
		{
			name:   "no envelope is untouched",
			stored: "plain text the user typed",
			want:   "plain text the user typed",
		},
		{
			// The composer store learned this in
			// TestMapComposerBubbleIndentedCodeAfterEnvelopeSurvives: a paste
			// that opens on an indented line loses its shape if the unwrap
			// trims past the envelope's own newlines.
			name:   "pasted code keeps its indentation",
			stored: "<user_query>\n    func main() {\n        return\n    }\n</user_query>",
			want:   "    func main() {\n        return\n    }",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, stamped := UnwrapUserEnvelope(testCase.stored)
			if got != testCase.want {
				t.Fatalf("text = %q, want %q", got, testCase.want)
			}
			if testCase.wantUTC == "" {
				if !stamped.IsZero() {
					t.Fatalf("timestamp = %v, want zero", stamped)
				}
				return
			}
			if stamped.UTC().Format(time.RFC3339) != testCase.wantUTC {
				t.Fatalf("timestamp = %v, want %s", stamped.UTC().Format(time.RFC3339), testCase.wantUTC)
			}
		})
	}
}

// TestUnwrapUserEnvelopeLeavesUnpairedMarkersAlone keeps the reader from
// editing text whose envelope is not the shape it understands. A user who
// pasted one marker into their own message keeps every byte they typed.
func TestUnwrapUserEnvelopeLeavesUnpairedMarkersAlone(t *testing.T) {
	t.Parallel()
	stored := "the closing marker is </user_query> in Cursor transcripts"
	got, stamped := UnwrapUserEnvelope(stored)
	if got != stored {
		t.Fatalf("text = %q, want it unchanged", got)
	}
	if !stamped.IsZero() {
		t.Fatalf("timestamp = %v, want zero", stamped)
	}
}

// TestUnwrapUserEnvelopeMakesRepeatedInjectionsIdentical is the reason this
// exists: Cursor's own injected prompt differs only by the time inside it, so
// nothing downstream can see two of them as the same message until the time
// moves out of the text.
func TestUnwrapUserEnvelopeMakesRepeatedInjectionsIdentical(t *testing.T) {
	t.Parallel()
	first, firstStamp := UnwrapUserEnvelope(
		"<timestamp>Sunday, Jul 26, 2026, 2:00 PM (UTC+2)</timestamp>\n\n" +
			"<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>")
	second, secondStamp := UnwrapUserEnvelope(
		"<timestamp>Sunday, Jul 26, 2026, 4:39 PM (UTC+2)</timestamp>\n\n" +
			"<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>")
	if first != second {
		t.Fatalf("two injections of one prompt differ:\n  %q\n  %q", first, second)
	}
	if firstStamp.Equal(secondStamp) {
		t.Fatalf("both timestamps = %v, want the two turn times preserved apart", firstStamp)
	}
}
