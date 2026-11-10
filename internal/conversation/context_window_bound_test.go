package conversation

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// hugeTurnParser yields turn-sized messages, which is what a Cursor conversation
// looks like once a turn's records are grouped into one message.
type hugeTurnParser struct {
	mu    sync.Mutex
	runes int
}

func (*hugeTurnParser) Provider() providerid.Provider { return providerid.ProviderCursor }

func (*hugeTurnParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (*hugeTurnParser) ScanRecord(string, FileStamp) (Record, bool) { return Record{}, false }

func (p *hugeTurnParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		for i := range 5 {
			msg := transcript.Message{
				Role:      "assistant",
				Timestamp: time.Unix(int64(i), 0),
				Text:      strings.Repeat("x", p.runes),
			}
			if !yield(msg, nil) {
				return
			}
		}
	}
}

func hugeTurnRecord() Record {
	return Record{
		ID:           "cursor:huge",
		Provider:     ProviderCursor,
		NativeID:     "huge",
		ArtifactPath: "/tmp/huge.jsonl",
		ArtifactKind: string(ArtifactKindCursorAgentTranscript),
	}
}

// TestContextWindowRenderIsBounded covers the two render call sites, which are
// the only changes that put the bounded shape into use. Grouping made one
// message a whole turn, and the largest in the observed corpus renders to
// roughly 300 KB, so an uncapped five-message window returned most of a
// megabyte where it used to return five short records.
func TestContextWindowRenderIsBounded(t *testing.T) {
	t.Parallel()

	const perMessageRunes = 300_000
	parser := &hugeTurnParser{mu: sync.Mutex{}, runes: perMessageRunes}
	registry := NewRegistry()
	registry.Register(parser)
	idx := newTestIndex(registry)

	// The index-centered path, which a search hit and --message-index both use.
	byIndex, err := idx.ContextWindowText(hugeTurnRecord(), "", 2, 2, 2, "")
	if err != nil {
		t.Fatalf("ContextWindowText by index returned error: %v", err)
	}
	if len([]rune(byIndex)) >= perMessageRunes {
		t.Fatalf("index-centered window is %d runes, want it bounded well under one message of %d",
			len([]rune(byIndex)), perMessageRunes)
	}

	// The timestamp-centered path, which renders from a materialized slice.
	byTimestamp, err := idx.ContextWindowText(hugeTurnRecord(), time.Unix(2, 0).Format(time.RFC3339), -1, 2, 2, "")
	if err != nil {
		t.Fatalf("ContextWindowText by timestamp returned error: %v", err)
	}
	if len([]rune(byTimestamp)) >= perMessageRunes {
		t.Fatalf("timestamp-centered window is %d runes, want it bounded well under one message of %d",
			len([]rune(byTimestamp)), perMessageRunes)
	}
}
