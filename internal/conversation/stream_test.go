package conversation

import (
	"context"
	"iter"
	"strconv"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// countingStreamParser yields a fixed number of messages lazily and records the
// highest message index a caller pulled, so a test can prove a window read stops
// before consuming the whole conversation.
type countingStreamParser struct {
	mu       sync.Mutex
	total    int
	maxPulls int
}

func (*countingStreamParser) Provider() providerid.Provider { return providerid.ProviderClaude }

func (*countingStreamParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (*countingStreamParser) ScanRecord(string, FileStamp) (Record, bool) {
	return Record{}, false
}

func (p *countingStreamParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		for i := 0; i < p.total; i++ {
			p.mu.Lock()
			if i+1 > p.maxPulls {
				p.maxPulls = i + 1
			}
			p.mu.Unlock()
			msg := transcript.Message{
				UUID:      strconv.Itoa(i),
				Role:      "user",
				Timestamp: time.Time{},
				Text:      "message " + strconv.Itoa(i),
				Thinking:  "",
				HasTools:  false,
				Tools:     nil,
			}
			if !yield(msg, nil) {
				return
			}
		}
	}
}

func newTestIndex(registry *Registry) *Index {
	return &Index{
		mu:           sync.Mutex{},
		registry:     registry,
		records:      nil,
		prevRecords:  nil,
		prevStamps:   nil,
		loaded:       false,
		refreshing:   false,
		lastRefresh:  time.Time{},
		cachePath:    "",
		debounce:     time.Hour,
		scanProvider: scan,
	}
}

func TestContextWindowByIndexStopsAtWindowEnd(t *testing.T) {
	t.Parallel()
	parser := &countingStreamParser{mu: sync.Mutex{}, total: 100000, maxPulls: 0}
	registry := NewRegistry()
	registry.Register(parser)
	idx := newTestIndex(registry)

	record := Record{
		ID:            "claude:stream",
		Provider:      ProviderClaude,
		NativeID:      "stream",
		Title:         "stream",
		WorkspaceRoot: "",
		ArtifactPath:  "/tmp/stream.jsonl",
		ArtifactKind:  "transcript",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}

	before := 2
	after := 3
	center := 10
	text, err := idx.ContextWindowText(record, "", center, before, after)
	if err != nil {
		t.Fatalf("context window: %v", err)
	}
	if text == "" {
		t.Fatalf("context window text was empty")
	}

	parser.mu.Lock()
	pulled := parser.maxPulls
	parser.mu.Unlock()

	wantEnd := center + after + 1
	if pulled != wantEnd {
		t.Fatalf("pulled %d messages, want exactly %d (window end); the read must stop before EOF", pulled, wantEnd)
	}
	if pulled >= parser.total {
		t.Fatalf("pulled %d messages, the window read consumed the whole conversation of %d", pulled, parser.total)
	}
}
