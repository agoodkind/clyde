package livetrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	r.Release(context.Background(), sess, "test.release")

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

// TestDrainIdleGraceForceClosesSilentSessions registers a handful of
// sessions, never touches them, and runs DrainWith with a small idle
// grace. The fast-path should fire and close every session before the
// poll loop ever ticks. The slog buffer is inspected to confirm the
// idle_force_closed event fires exactly once with the correct count.
func TestDrainIdleGraceForceClosesSilentSessions(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	guard := &syncWriter{w: buf}
	handler := slog.NewJSONHandler(guard, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.idle_grace.silent",
		Log:         logger,
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 100 * time.Millisecond,
	})
	const sessionCount = 4
	closers := make([]*noopCloser, sessionCount)
	for i := range closers {
		closers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		_, err := r.Register(context.Background(), "silent", testMeta{Tag: fmt.Sprintf("silent-%d", i)}, closers[i])
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	// Wait long enough that every session's idle exceeds the grace.
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	result := r.DrainWith(ctx, "test.idle_grace.silent", DrainOptions{IdleGrace: 5 * time.Millisecond})
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("drain elapsed %v, want < 200ms (fast-path should evict immediately)", elapsed)
	}
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.ForceClosed != sessionCount {
		t.Fatalf("force-closed: got %d, want %d", result.ForceClosed, sessionCount)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("closer %d: got %d, want 1", i, got)
		}
	}
	tally := tallyMessages(t, buf.Bytes())
	if tally["livetrack.drain.idle_force_closed"] != 1 {
		t.Fatalf("idle_force_closed events: got %d, want 1 (all=%v)", tally["livetrack.drain.idle_force_closed"], tally)
	}
}

// TestDrainIdleGraceLetsActiveSessionsRun registers sessions, drives
// a goroutine that touches them every millisecond, and asserts the
// idle fast-path does not fire because every session has a fresh
// last-activity timestamp at drain start. The drain still completes
// via the deadline path because the closers block until ctx fires.
func TestDrainIdleGraceLetsActiveSessionsRun(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	guard := &syncWriter{w: buf}
	handler := slog.NewJSONHandler(guard, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.idle_grace.active",
		Log:         logger,
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	const sessionCount = 3
	closers := make([]*blockingCloser, sessionCount)
	sessions := make([]*Session[testMeta], sessionCount)
	for i := range closers {
		closers[i] = newBlockingCloser()
		sess, err := r.Register(context.Background(), "active", testMeta{Tag: fmt.Sprintf("active-%d", i)}, closers[i])
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		sessions[i] = sess
	}
	stopTouching := make(chan struct{})
	touchingDone := make(chan struct{})
	go func() {
		defer close(touchingDone)
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTouching:
				return
			case <-ticker.C:
				for _, sess := range sessions {
					sess.Touch()
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	drainDone := make(chan DrainResult, 1)
	go func() {
		drainDone <- r.DrainWith(ctx, "test.idle_grace.active", DrainOptions{IdleGrace: 50 * time.Millisecond})
	}()
	// Release closers a touch after the deadline so deadline-path
	// force-close gets to fire.
	time.AfterFunc(220*time.Millisecond, func() {
		for _, closer := range closers {
			close(closer.release)
		}
	})
	result := <-drainDone
	close(stopTouching)
	<-touchingDone
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.ForceClosed != sessionCount {
		t.Fatalf("force-closed: got %d, want %d (deadline path expected)", result.ForceClosed, sessionCount)
	}
	tally := tallyMessages(t, buf.Bytes())
	if tally["livetrack.drain.idle_force_closed"] != 0 {
		t.Fatalf("idle_force_closed events: got %d, want 0 (active sessions should escape the fast-path)", tally["livetrack.drain.idle_force_closed"])
	}
}

// TestDrainMixedSessions registers a mix of silent and active
// sessions and confirms the fast-path evicts only the silent ones
// while the active ones survive to the deadline-path force-close.
func TestDrainMixedSessions(t *testing.T) {
	t.Parallel()
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.idle_grace.mixed",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	silentCount := 2
	activeCount := 2
	silentClosers := make([]*noopCloser, silentCount)
	for i := range silentClosers {
		silentClosers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		_, err := r.Register(context.Background(), "silent", testMeta{Tag: fmt.Sprintf("silent-%d", i)}, silentClosers[i])
		if err != nil {
			t.Fatalf("register silent %d: %v", i, err)
		}
	}
	activeClosers := make([]*blockingCloser, activeCount)
	activeSessions := make([]*Session[testMeta], activeCount)
	for i := range activeClosers {
		activeClosers[i] = newBlockingCloser()
		sess, err := r.Register(context.Background(), "active", testMeta{Tag: fmt.Sprintf("active-%d", i)}, activeClosers[i])
		if err != nil {
			t.Fatalf("register active %d: %v", i, err)
		}
		activeSessions[i] = sess
	}
	// Sleep so the silent sessions age past the grace. Then touch the
	// active ones immediately before Drain so their lastActivity is
	// fresh, and keep touching while the drain runs.
	time.Sleep(20 * time.Millisecond)
	for _, sess := range activeSessions {
		sess.Touch()
	}
	stopTouching := make(chan struct{})
	touchingDone := make(chan struct{})
	go func() {
		defer close(touchingDone)
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTouching:
				return
			case <-ticker.C:
				for _, sess := range activeSessions {
					sess.Touch()
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	drainDone := make(chan DrainResult, 1)
	go func() {
		drainDone <- r.DrainWith(ctx, "test.idle_grace.mixed", DrainOptions{IdleGrace: 5 * time.Millisecond})
	}()
	time.AfterFunc(170*time.Millisecond, func() {
		for _, closer := range activeClosers {
			close(closer.release)
		}
	})
	result := <-drainDone
	close(stopTouching)
	<-touchingDone
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.ForceClosed != silentCount+activeCount {
		t.Fatalf("force-closed total: got %d, want %d", result.ForceClosed, silentCount+activeCount)
	}
	for i, closer := range silentClosers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("silent closer %d: got %d, want 1 (fast-path expected)", i, got)
		}
	}
	for i, closer := range activeClosers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("active closer %d: got %d, want 1 (deadline-path expected)", i, got)
		}
	}
}

// TestDrainIdleGraceZeroSkipsFastPath registers silent sessions but
// drains with IdleGrace zero. The fast-path event must not fire and
// the drain must use the legacy wait-then-deadline shape. Closers
// release naturally so the idle path completes without a force-close.
func TestDrainIdleGraceZeroSkipsFastPath(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	guard := &syncWriter{w: buf}
	handler := slog.NewJSONHandler(guard, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)
	r := New[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.idle_grace.zero",
		Log:         logger,
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	const sessionCount = 3
	closers := make([]*noopCloser, sessionCount)
	sessions := make([]*Session[testMeta], sessionCount)
	for i := range closers {
		closers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		sess, err := r.Register(context.Background(), "zero", testMeta{Tag: fmt.Sprintf("zero-%d", i)}, closers[i])
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		sessions[i] = sess
	}
	go func() {
		for _, sess := range sessions {
			r.Release(context.Background(), sess, "test.idle_grace.zero")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	result := r.DrainWith(ctx, "test.idle_grace.zero", DrainOptions{IdleGrace: 0})
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.ForceClosed != 0 {
		t.Fatalf("force-closed: got %d, want 0 (no fast-path, releases idle out)", result.ForceClosed)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 0 {
			t.Fatalf("closer %d: got %d, want 0 (release happens via Release, not force)", i, got)
		}
	}
	tally := tallyMessages(t, buf.Bytes())
	if tally["livetrack.drain.idle_force_closed"] != 0 {
		t.Fatalf("idle_force_closed events: got %d, want 0", tally["livetrack.drain.idle_force_closed"])
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
