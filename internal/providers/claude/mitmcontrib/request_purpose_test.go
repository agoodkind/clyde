package mitmcontrib

import (
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

// TestClassifyRequestPurpose pins the mapping from Claude Code's request-class
// header to the generic purpose. The class values come from a survey of the
// MITM capture store: 1,173 /v1/messages requests carried auxiliary, main,
// subagent, or compaction.
func TestClassifyRequestPurpose(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		class string
		want  mitm.RequestPurpose
	}{
		{"manual compaction", "compaction", mitm.RequestPurposeCompaction},
		{"automatic compaction uses the same class", "compaction", mitm.RequestPurposeCompaction},
		{"mixed case is accepted", "Compaction", mitm.RequestPurposeCompaction},
		{"surrounding space is trimmed", " compaction ", mitm.RequestPurposeCompaction},
		{"ordinary turn", "main", mitm.RequestPurposeUnspecified},
		{"auxiliary turn", "auxiliary", mitm.RequestPurposeUnspecified},
		{"subagent turn", "subagent", mitm.RequestPurposeUnspecified},
		{"unknown future class", "something-new", mitm.RequestPurposeUnspecified},
		{"empty class", "", mitm.RequestPurposeUnspecified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			if tc.class != "" {
				headers.Set("x-claude-code-request-class", tc.class)
			}
			if got := classifyRequestPurpose(headers); got != tc.want {
				t.Fatalf("classifyRequestPurpose(%q) = %q, want %q", tc.class, got, tc.want)
			}
		})
	}
}

// TestClassifyRequestPurposeWithoutHeader covers a request from a client that
// sets no request class at all.
func TestClassifyRequestPurposeWithoutHeader(t *testing.T) {
	t.Parallel()
	if got := classifyRequestPurpose(http.Header{}); got != mitm.RequestPurposeUnspecified {
		t.Fatalf("classifyRequestPurpose(no header) = %q, want unspecified", got)
	}
}

// TestRouteProviderImplementsRequestClassifier asserts the registered provider
// satisfies the optional extension, because the generic layer reaches the
// classifier only through that interface.
func TestRouteProviderImplementsRequestClassifier(t *testing.T) {
	t.Parallel()
	var provider mitm.Provider = routeProvider{}
	classifier, ok := provider.(mitm.RequestClassifier)
	if !ok {
		t.Fatal("routeProvider must implement mitm.RequestClassifier")
	}
	headers := http.Header{}
	headers.Set("x-claude-code-request-class", "compaction")
	if got := classifier.ClassifyRequestPurpose(headers); got != mitm.RequestPurposeCompaction {
		t.Fatalf("ClassifyRequestPurpose = %q, want compaction", got)
	}
}
