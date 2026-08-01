package cursorjsonl

import "testing"

// TestUnwrapUserEnvelopeEmptyQueryBody covers the turn where the user typed
// nothing and only attached context, which Cursor writes as an open marker, one
// newline, and the close marker. Both query patterns claim that newline, so the
// open match ends one byte past where the close match starts, and slicing the
// body between them inverts. The deployed daemon panicked on exactly this shape
// with "slice bounds out of range [345:344]".
//
// The envelope is well formed, so its markers still come out and the empty body
// comes back empty. Leaving the markers in would put text the user never saw
// into the search corpus.
func TestUnwrapUserEnvelopeEmptyQueryBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "newline body",
			input: "<user_query>\n</user_query>",
			want:  "",
		},
		{
			name:  "newline body after an attachment",
			input: "<attached>notes.md</attached>\n<user_query>\n</user_query>",
			want:  "<attached>notes.md</attached>\n",
		},
		{
			name:  "blank line body",
			input: "<user_query>\n\n</user_query>",
			want:  "",
		},
		{
			name:  "no body at all",
			input: "<user_query></user_query>",
			want:  "",
		},
		{
			name:  "body of spaces",
			input: "<user_query>\n   </user_query>",
			want:  "",
		},
		{
			name:  "text survives after the envelope",
			input: "<user_query>\n</user_query>trailing",
			want:  "trailing",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, _ := UnwrapUserEnvelope(testCase.input)
			if got != testCase.want {
				t.Fatalf("UnwrapUserEnvelope(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}

// TestUnwrapUserEnvelopeKeepsTypedQuery pins the ordinary case beside the empty
// one, so a fix for the inverted slice cannot quietly widen what it removes.
func TestUnwrapUserEnvelopeKeepsTypedQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "one line",
			input: "<user_query>\nhello\n</user_query>",
			want:  "hello",
		},
		{
			name:  "indented paste keeps its shape",
			input: "<user_query>\n    indented\n</user_query>",
			want:  "    indented",
		},
		{
			name:  "attachment before the query",
			input: "<attached>a.go</attached>\n<user_query>\nwhy\n</user_query>",
			want:  "<attached>a.go</attached>\nwhy",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, _ := UnwrapUserEnvelope(testCase.input)
			if got != testCase.want {
				t.Fatalf("UnwrapUserEnvelope(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}
