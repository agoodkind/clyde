package livetrack

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlogEventCoverage exercises every event type by routing them
// through a JSON handler and counting messages.
func TestSlogEventCoverage(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	guard := &syncWriter{w: buf}
	handler := slog.NewJSONHandler(guard, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		AddSource:   false,
		ReplaceAttr: nil,
	})
	logger := slog.New(handler)
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.slog.events",
		Log:         logger,
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	closer := &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
	sess, err := r.Register(context.Background(), "slog", testMeta{Tag: "slog"}, closer)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r.Release(sess, "test.release")

	// Force-close path: register a fresh blocker, drain with a tight
	// deadline so the force-close event fires.
	blocker := newBlockingCloser()
	_, err = r.Register(context.Background(), "force", testMeta{Tag: "force"}, blocker)
	if err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(blocker.release)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	r.Drain(ctx, "test.drain")

	wantMessages := map[string]int{
		"livetrack.session.registered":  2,
		"livetrack.session.released":    1,
		"livetrack.session.force_close": 1,
		"livetrack.drain.started":       1,
		"livetrack.drain.complete":      1,
	}
	got := tallyMessages(t, buf.Bytes())
	for message, want := range wantMessages {
		if got[message] != want {
			t.Errorf("message %q: got %d, want %d (all=%v)", message, got[message], want, got)
		}
	}
}

func TestDrainAlreadyDraining(t *testing.T) {
	t.Parallel()
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.dup",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	blocker := newBlockingCloser()
	_, err := r.Register(context.Background(), "dup", testMeta{Tag: "dup"}, blocker)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	firstDone := make(chan DrainResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		firstDone <- r.Drain(ctx, "first")
	}()
	// Wait until Drain advances out of Open before calling again.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.State() != StateOpen {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	dup := r.Drain(context.Background(), "second")
	if dup.Reason != "already_draining" {
		t.Fatalf("duplicate reason: got %q, want already_draining", dup.Reason)
	}
	close(blocker.release)
	<-firstDone
}

func TestDescendantsOf(t *testing.T) {
	t.Parallel()
	r := New[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.descendants",
	})
	root, err := r.Register(context.Background(), "root", testMeta{Tag: "root"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register root: %v", err)
	}
	child, err := r.Register(context.Background(), "child", testMeta{Tag: "child"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}, WithParent[testMeta](root))
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	_, err = r.Register(context.Background(), "grandchild", testMeta{Tag: "grandchild"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}, WithParent[testMeta](child))
	if err != nil {
		t.Fatalf("register grandchild: %v", err)
	}
	descendants := r.DescendantsOf(root.ID)
	if len(descendants) != 2 {
		t.Fatalf("descendants: got %d, want 2", len(descendants))
	}
	if r.ChildrenOf("") != nil {
		t.Fatalf("ChildrenOf empty: expected nil")
	}
	if r.DescendantsOf("") != nil {
		t.Fatalf("DescendantsOf empty: expected nil")
	}
}

func TestForEachAndSnapshot(t *testing.T) {
	t.Parallel()
	r := New[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.foreach",
	})
	for i := 0; i < 4; i++ {
		_, err := r.Register(context.Background(), "fe", testMeta{Tag: "fe"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	count := 0
	r.ForEach(func(s Session[testMeta]) {
		count++
		_ = s.Meta.Tag
	})
	if count != 4 {
		t.Fatalf("ForEach count: got %d, want 4", count)
	}
	snapshot := r.Snapshot()
	if len(snapshot) != 4 {
		t.Fatalf("Snapshot count: got %d, want 4", len(snapshot))
	}
}

// syncWriter serializes writes from concurrent slog goroutines so the
// JSON record stream stays parseable in tests.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func tallyMessages(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	tally := make(map[string]int)
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode slog line %q: %v", line, err)
		}
		tally[record.Msg]++
	}
	return tally
}
