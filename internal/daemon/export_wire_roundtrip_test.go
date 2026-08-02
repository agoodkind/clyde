package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// TestExportContentKindsSurviveTheWire proves the export selection round-trips
// the control socket intact: every selectable content kind that goes into the
// request's per-kind booleans comes back out of the daemon-side rebuild. The
// injected kind shipped without its wire field once, so the daemon stripped
// hook content even when the caller asked for it; this test fails the moment a
// kind exists in the taxonomy without a wire mapping.
func TestExportContentKindsSurviveTheWire(t *testing.T) {
	t.Parallel()
	for _, kind := range conversation.AllContentKinds {
		set := conversation.NewContentKindSet(kind)
		options := conversation.ExportOptions{
			Format:       conversation.ExportFormatMarkdown,
			HistoryStart: 0,
			LastN:        0,
			MaxLines:     0,
			MaxTokens:    "",
			TokenModel:   "",
			Whitespace:   conversation.WhitespacePreserve,
			Content:      set,
			Compaction: conversation.CompactionExportOptions{
				IncludeSelector: "",
				FullHistory:     false,
			},
		}
		request := exportTranscriptRequest("claude:wire-test", options)
		rebuilt := contentKindSetFromExportRequest(request)
		if !rebuilt.Has(kind) {
			t.Errorf("content kind %q was lost crossing the export wire", kind)
		}
		// The rebuilt set must also not invent kinds the caller never named,
		// modulo the deliberate tool-kind collapse both sides share.
		for _, other := range conversation.AllContentKinds {
			if other == kind {
				continue
			}
			if rebuilt.Has(other) && !set.Has(other) {
				t.Errorf("selecting %q surfaced %q on the daemon side", kind, other)
			}
		}
	}
}
