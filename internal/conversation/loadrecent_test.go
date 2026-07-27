package conversation

import (
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// fakeTailParser is a [Parser] that also implements [TailParser], so tests
// can control exactly what TailSize and StreamFrom report without depending
// on a real provider's artifact format. It records every start offset
// StreamFrom was called with, so a test can assert the growth loop stayed
// bounded rather than only checking the final reply.
type fakeTailParser struct {
	streamFuncParser
	mu           sync.Mutex
	size         int64
	sizeOK       bool
	startsCalled []int64
	streamFrom   func(opts LoadOptions, start int64, end int64) iter.Seq2[transcript.Message, error]
}

func (p *fakeTailParser) TailSize(string) (int64, bool) {
	return p.size, p.sizeOK
}

func (p *fakeTailParser) StreamFrom(
	_ string,
	opts LoadOptions,
	start int64,
	end int64,
) iter.Seq2[transcript.Message, error] {
	p.mu.Lock()
	p.startsCalled = append(p.startsCalled, start)
	p.mu.Unlock()
	return p.streamFrom(opts, start, end)
}

func (p *fakeTailParser) recordedStarts() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.startsCalled...)
}

var _ TailParser = (*fakeTailParser)(nil)

func loadRecentTestRecord(id string) Record {
	return Record{
		ID:            id,
		Provider:      ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}

func fixedMessageStream(messages []transcript.Message) func(LoadOptions, int64, int64) iter.Seq2[transcript.Message, error] {
	return func(LoadOptions, int64, int64) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range messages {
				if !yield(message, nil) {
					return
				}
			}
		}
	}
}

func conversationalMessage(uuid string, text string, timestampUnix int64) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "user",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Unix(timestampUnix, 0),
		Text:              text,
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

// TestLoadRecentTurnsFallsBackWithoutTailParser proves LoadRecentTurns
// reproduces LoadMessagesWithOptions's reply exactly when the registered
// parser does not implement TailParser, which is the case for every
// provider that has not opted into a bounded read (Zed, and Cursor's
// composer and legacy artifact kinds).
func TestLoadRecentTurnsFallsBackWithoutTailParser(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range optionAwareStream(opts) {
				if !yield(message, nil) {
					return
				}
			}
		}
	}})
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("no-tail-parser")

	full, err := idx.LoadMessagesWithOptions(record, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadMessagesWithOptions: %v", err)
	}
	recent, err := idx.LoadRecentTurns(record, 1, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(recent) != len(full) {
		t.Fatalf("LoadRecentTurns returned %d messages, want %d (the full fallback reply)", len(recent), len(full))
	}
	for i := range full {
		if recent[i].UUID != full[i].UUID || recent[i].Text != full[i].Text {
			t.Fatalf("recent[%d] = %#v, want %#v", i, recent[i], full[i])
		}
	}
}

// TestLoadRecentTurnsFallsBackWhenTailSizeReportsUnsupported proves an
// artifact TailSize reports as not byte-addressable (ok=false) falls back to
// the full load. This is the shape of Zed's whole-document Stream and
// Cursor's composer/legacy kinds, none of which are byte-addressable JSONL.
func TestLoadRecentTurnsFallsBackWhenTailSizeReportsUnsupported(t *testing.T) {
	t.Parallel()
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				for _, message := range optionAwareStream(opts) {
					if !yield(message, nil) {
						return
					}
				}
			}
		}},
		mu:           sync.Mutex{},
		size:         0,
		sizeOK:       false,
		startsCalled: nil,
		streamFrom:   fixedMessageStream(nil),
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("not-byte-addressable")

	recent, err := idx.LoadRecentTurns(record, 1, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(recent) != 1 || recent[0].Text != "visible transcript message" {
		t.Fatalf("LoadRecentTurns = %#v, want the full-load fallback reply", recent)
	}
	if len(fake.recordedStarts()) != 0 {
		t.Fatalf("StreamFrom was called %v times, want 0 when TailSize reports ok=false", fake.recordedStarts())
	}
}

// TestLoadRecentTurnsFallsBackWithIncludeToolOutputs proves the
// IncludeToolOutputs guard: both providers' streamWithToolOutputs equivalent
// attaches a tool call's output from a later line, which a bounded suffix
// read cannot see, so LoadRecentTurns must fall back to the full load without
// ever calling StreamFrom.
func TestLoadRecentTurnsFallsBackWithIncludeToolOutputs(t *testing.T) {
	t.Parallel()
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				for _, message := range optionAwareStream(opts) {
					if !yield(message, nil) {
						return
					}
				}
			}
		}},
		mu:           sync.Mutex{},
		size:         1 << 20,
		sizeOK:       true,
		startsCalled: nil,
		streamFrom:   fixedMessageStream(nil),
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("include-tool-outputs")

	recent, err := idx.LoadRecentTurns(record, 1, LoadOptions{IncludeToolOutputs: true})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(recent) != 1 || recent[0].Text != "visible transcript message" {
		t.Fatalf("LoadRecentTurns = %#v, want the full-load fallback reply", recent)
	}
	if len(fake.recordedStarts()) != 0 {
		t.Fatalf("StreamFrom was called %v times, want 0 when IncludeToolOutputs is set", fake.recordedStarts())
	}
}

// TestLoadRecentTurnsUsesTailParserWhenEnoughFound proves LoadRecentTurns
// stops growing the window and returns the reply verbatim, without ever
// falling back to a full load, once one StreamFrom call already reports
// enough qualifying messages.
func TestLoadRecentTurnsUsesTailParserWhenEnoughFound(t *testing.T) {
	t.Parallel()
	tailMessages := []transcript.Message{conversationalMessage("tail-only", "from the tail reader, not the full loader", 9)}
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				panic("full Stream should not be called when the tail window already has enough messages")
			}
		}},
		mu:           sync.Mutex{},
		size:         1 << 20,
		sizeOK:       true,
		startsCalled: nil,
		streamFrom:   fixedMessageStream(tailMessages),
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("enough-on-first-try")

	recent, err := idx.LoadRecentTurns(record, 1, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(recent) != 1 || recent[0].UUID != "tail-only" {
		t.Fatalf("LoadRecentTurns = %#v, want the StreamFrom reply verbatim", recent)
	}
	starts := fake.recordedStarts()
	if len(starts) != 1 {
		t.Fatalf("StreamFrom called %d times, want exactly 1", len(starts))
	}
	if starts[0] == 0 {
		t.Fatalf("StreamFrom's first call started at offset 0 on a 1MB fixture, want a bounded window near the end")
	}
}

// TestLoadRecentTurnsGrowsWindowUntilEnoughFound proves the geometric growth
// loop: when an early, narrow window comes up short, LoadRecentTurns retries
// with a wider one anchored at the same fixed end, rather than falling back
// to a full load or giving up. The offsets recorded here are the load-bearing
// assertion: output equality alone would also pass a version that always
// read from offset 0.
func TestLoadRecentTurnsGrowsWindowUntilEnoughFound(t *testing.T) {
	t.Parallel()
	const wantNeed = 3
	callCount := 0
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				panic("full Stream should not be called once a wide-enough window is found")
			}
		}},
		mu:           sync.Mutex{},
		size:         1 << 30, // 1GiB, far bigger than the initial window
		sizeOK:       true,
		startsCalled: nil,
		streamFrom:   nil,
	}
	fake.streamFrom = func(opts LoadOptions, start int64, end int64) iter.Seq2[transcript.Message, error] {
		callCount++
		return func(yield func(transcript.Message, error) bool) {
			// Report enough messages only from the third call onward, forcing
			// two growth steps before the loop is satisfied.
			count := 1
			if callCount >= 3 {
				count = wantNeed
			}
			for i := range count {
				if !yield(conversationalMessage("m", "turn", int64(i)), nil) {
					return
				}
			}
		}
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("grows-window")

	recent, err := idx.LoadRecentTurns(record, wantNeed, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	if len(recent) != wantNeed {
		t.Fatalf("LoadRecentTurns returned %d messages, want %d", len(recent), wantNeed)
	}
	starts := fake.recordedStarts()
	if len(starts) != 3 {
		t.Fatalf("StreamFrom called %d times, want exactly 3 growth steps", len(starts))
	}
	if starts[0] <= starts[1] || starts[1] <= starts[2] {
		t.Fatalf("starts = %v, want strictly decreasing (each retry widens the window toward offset 0)", starts)
	}
}

// TestLoadRecentTurnsPropagatesStreamFromError proves a StreamFrom error is
// surfaced rather than silently falling back to a full load, which would
// hide a real read failure behind a slow, misleading success.
func TestLoadRecentTurnsPropagatesStreamFromError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {}
		}},
		mu:     sync.Mutex{},
		size:   1 << 20,
		sizeOK: true,
		streamFrom: func(LoadOptions, int64, int64) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				yield(transcript.Message{}, wantErr)
			}
		},
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("stream-from-error")

	_, err := idx.LoadRecentTurns(record, 1, LoadOptions{})
	if err == nil {
		t.Fatalf("LoadRecentTurns returned nil error, want the StreamFrom failure surfaced")
	}
}

// TestLoadRecentTurnsNeedZeroOrNegativeFallsBack proves a non-positive need
// short-circuits straight to the full load rather than dividing by, or
// looping on, a meaningless budget.
func TestLoadRecentTurnsNeedZeroOrNegativeFallsBack(t *testing.T) {
	t.Parallel()
	fake := &fakeTailParser{
		streamFuncParser: streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
			return func(yield func(transcript.Message, error) bool) {
				for _, message := range optionAwareStream(opts) {
					if !yield(message, nil) {
						return
					}
				}
			}
		}},
		mu:           sync.Mutex{},
		size:         1 << 20,
		sizeOK:       true,
		startsCalled: nil,
		streamFrom:   fixedMessageStream(nil),
	}
	registry := NewRegistry()
	registry.Register(fake)
	idx := newTestIndex(registry)
	record := loadRecentTestRecord("need-non-positive")

	for _, need := range []int{0, -1} {
		recent, err := idx.LoadRecentTurns(record, need, LoadOptions{})
		if err != nil {
			t.Fatalf("LoadRecentTurns(need=%d): %v", need, err)
		}
		if len(recent) != 1 || recent[0].Text != "visible transcript message" {
			t.Fatalf("LoadRecentTurns(need=%d) = %#v, want the full-load fallback reply", need, recent)
		}
	}
	if len(fake.recordedStarts()) != 0 {
		t.Fatalf("StreamFrom was called %v times, want 0 for a non-positive need", fake.recordedStarts())
	}
}
