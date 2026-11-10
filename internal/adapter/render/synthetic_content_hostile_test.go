package render

import (
	"strings"
	"testing"
	"time"
)

// TestExtractSyntheticPartsSurvivesHostileMarkers runs the marker shapes a
// crafted or malformed assistant turn can carry. This code parses bytes another
// program wrote, and the conversation readers run it on every stored turn, so a
// panic here stops a whole ingestion pass rather than costing one message.
//
// The bar is that the call returns. Losing a marker costs the turn its
// decoration; panicking costs the pass.
func TestExtractSyntheticPartsSurvivesHostileMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"open marker alone", "<!--clyde-thinking-->"},
		{"close marker alone", "<!--/clyde-thinking-->"},
		{"reversed markers", "<!--/clyde-thinking-->body<!--clyde-thinking-->"},
		{"markers with nothing between", "<!--clyde-thinking--><!--/clyde-thinking-->"},
		{"ordinary pair", "<!--clyde-thinking-->reasoning<!--/clyde-thinking-->"},
		// A close marker inside an open marker's attribute value makes the open
		// match span the close, so the body span between them inverts. Both
		// optional open attributes accept any byte but a double quote.
		{
			"close marker inside data-ref",
			`<!--clyde-thinking data-ref="a<!--/clyde-thinking-->b"-->body<!--/clyde-thinking-->`,
		},
		{
			"close marker inside data-origin",
			`<!--clyde-thinking data-origin="x<!--/clyde-thinking-->y"-->body<!--/clyde-thinking-->`,
		},
		{
			"close marker inside both open attributes",
			`<!--clyde-thinking data-ref="a<!--/clyde-thinking-->b" data-origin="c<!--/clyde-thinking-->d"-->body<!--/clyde-thinking-->`,
		},
		{
			"open marker inside a close attribute",
			`<!--clyde-thinking-->body<!--/clyde-thinking data-signature="s<!--clyde-thinking-->t"-->`,
		},
		{"nested pairs", "<!--clyde-thinking--><!--clyde-thinking-->inner<!--/clyde-thinking--><!--/clyde-thinking-->"},
		{"many open markers", strings.Repeat("<!--clyde-thinking-->", 500)},
		{"many close markers", strings.Repeat("<!--/clyde-thinking-->", 500)},
		{"alternating markers", strings.Repeat("<!--clyde-thinking--><!--/clyde-thinking-->", 500)},
		{"unterminated open marker", "<!--clyde-thinking data-ref=\"unclosed"},
		{"invalid utf8 body", "<!--clyde-thinking-->\xff\xfe\x00<!--/clyde-thinking-->"},
		{"very long body", "<!--clyde-thinking-->" + strings.Repeat("a", 100000) + "<!--/clyde-thinking-->"},
		{"notice kind", "<!--clyde-notice data-ref=\"a<!--/clyde-notice-->b\"-->body<!--/clyde-notice-->"},
		{
			"redacted thinking kind",
			`<!--clyde-redacted-thinking data-ref="a<!--/clyde-redacted-thinking-->b"-->body<!--/clyde-redacted-thinking-->`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = ExtractSyntheticParts(testCase.input)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("ExtractSyntheticParts did not return within 5s")
			}
		})
	}
}

// TestExtractSyntheticPartsNeverGrowsTheText states the property that keeps a
// malformed marker from turning into more content than arrived.
func TestExtractSyntheticPartsNeverGrowsTheText(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"",
		"<!--clyde-thinking-->reasoning<!--/clyde-thinking-->",
		`<!--clyde-thinking data-ref="a<!--/clyde-thinking-->b"-->body<!--/clyde-thinking-->`,
		"<!--/clyde-thinking-->body<!--clyde-thinking-->",
		strings.Repeat("<!--clyde-thinking-->", 50),
	}
	for _, input := range inputs {
		total := 0
		for _, part := range ExtractSyntheticParts(input) {
			total += len(part.Body)
		}
		if total > len(input) {
			t.Fatalf("input %q of %d bytes yielded %d bytes of parts", input, len(input), total)
		}
	}
}
