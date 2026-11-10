package cursorjsonl

import (
	"strings"
	"testing"
	"time"
)

// TestUnwrapUserEnvelopeSurvivesHostileInput runs the shapes a malformed or
// crafted transcript can carry. The reader parses bytes another program wrote,
// so every one of these reaches it eventually: a truncated file, a message the
// user pasted a transcript into, or a store written by a Cursor version that
// framed turns differently.
//
// The bar is that the call returns. Losing a timestamp costs the turn its time;
// hanging costs the whole index refresh.
func TestUnwrapUserEnvelopeSurvivesHostileInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"open marker alone", "<user_query>"},
		{"close marker alone", "</user_query>"},
		{"reversed markers", "</user_query>text<user_query>"},
		{"markers with nothing between", "<user_query></user_query>"},
		{"nested envelopes", "<user_query>\n<user_query>inner</user_query>\n</user_query>"},
		{"many open markers", strings.Repeat("<user_query>", 500)},
		{"many close markers", strings.Repeat("</user_query>", 500)},
		{"alternating markers", strings.Repeat("<user_query></user_query>", 500)},
		{"unterminated timestamp", "<timestamp>Monday, Jan 1"},
		{"empty timestamp", "<timestamp></timestamp>text"},
		{"timestamp holding a tag", "<timestamp><user_query></timestamp>body"},
		{"timestamp with a huge value", "<timestamp>" + strings.Repeat("9", 100000) + "</timestamp>body"},
		{"zone with no hours", "<timestamp>Monday, Jan 1, 2026, 1:00 AM (UTC+)</timestamp>x"},
		{"zone hours out of range", "<timestamp>Monday, Jan 1, 2026, 1:00 AM (UTC+99)</timestamp>x"},
		{"zone minutes out of range", "<timestamp>Monday, Jan 1, 2026, 1:00 AM (UTC+5:99)</timestamp>x"},
		{"whitespace flood before the marker", strings.Repeat("\n", 100000) + "<user_query>x</user_query>"},
		{"invalid utf8", "<user_query>\xff\xfe\x00</user_query>"},
		{"lone surrogate bytes", "<user_query>\xed\xa0\x80</user_query>"},
		{"very long body", "<user_query>" + strings.Repeat("a", 1000000) + "</user_query>"},
		{"timestamp only", "<timestamp>Monday, Jan 1, 2026, 1:00 AM (UTC+0)</timestamp>"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = UnwrapUserEnvelope(testCase.input)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("UnwrapUserEnvelope did not return within 5s")
			}
		})
	}
}

// TestUnwrapUserEnvelopeNeverGrowsTheText states the property that keeps a
// malformed envelope from turning into more content than arrived. Every shape
// above either loses envelope bytes or returns the input as it stands.
func TestUnwrapUserEnvelopeNeverGrowsTheText(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"",
		"<user_query>body</user_query>",
		"</user_query>text<user_query>",
		"<user_query>\n<user_query>inner</user_query>\n</user_query>",
		"<timestamp>Monday, Jan 1, 2026, 1:00 AM (UTC+0)</timestamp>\n\n<user_query>x</user_query>",
		strings.Repeat("<user_query>", 50),
	}
	for _, input := range inputs {
		got, _ := UnwrapUserEnvelope(input)
		if len(got) > len(input) {
			t.Fatalf("input %q grew to %q", input, got)
		}
	}
}
