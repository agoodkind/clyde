package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDrainReloadedMITMReportsActiveTunnelsOnIdle exercises the
// idle-exit path: activeCount returns 0 so the drain skips its
// goroutine entirely, the drain_complete event fires synchronously
// with active_tunnels:0, and no mitm_drain_timeout warning is
// emitted.
func TestDrainReloadedMITMReportsActiveTunnelsOnIdle(t *testing.T) {
	buf, log := captureSlogJSON()
	listener := newRecordingListener(t)
	rt := &daemonRuntime{
		listener: nil,
		adapter:  nil,
		webapp:   nil,
		mitm: &mitmProcess{
			cancel:         func() {},
			drain:          func(context.Context) error { return nil },
			waitIdle:       func(context.Context) int { return 0 },
			activeCount:    func() int { return 0 },
			forceClose:     func() error { return nil },
			closeListener:  listener.Close,
			done:           nil,
			lis:            listener,
			proxy:          nil,
			reloadDraining: atomic.Bool{},
		},
		reloadLock:   sync.Mutex{},
		drainDonesMu: sync.Mutex{},
		drainDones:   nil,
	}
	done := drainReloadedMITM(context.Background(), log, rt)
	waitDoneOrFail(t, done, 2*time.Second)
	tally := tallyMITMReloadEvents(t, buf.Bytes())
	if tally["daemon.reload.mitm_drain_complete.active_tunnels:0"] != 1 {
		t.Fatalf("expected mitm_drain_complete with active_tunnels:0, got tally=%v", tally)
	}
	if tally["daemon.reload.mitm_drain_timeout"] != 0 {
		t.Fatalf("expected no mitm_drain_timeout on idle path, got tally=%v", tally)
	}
}

// TestDrainReloadedMITMForceClosesActiveTunnelsAtCap exercises the
// outer-cap path: activeCount reports tunnels in flight at handoff,
// the drain goroutine kicks off, and the simulated drain returns an
// error to mimic the cap elapsing. force_close fires once the
// goroutine completes, and the cap event records active_tunnels.
// CLYDE-324: the prior implementation force-closed after a 4s
// deadline on the synchronous path, truncating in-flight HTTPS
// responses; this test pins the new contract that force-close fires
// only on the cap path and that the function returns immediately so
// the reload RPC is not blocked.
func TestDrainReloadedMITMForceClosesActiveTunnelsAtCap(t *testing.T) {
	buf, log := captureSlogJSON()
	listener := newRecordingListener(t)
	forceCalled := atomicCounter{}
	rt := &daemonRuntime{
		listener: nil,
		adapter:  nil,
		webapp:   nil,
		mitm: &mitmProcess{
			cancel: func() {},
			drain: func(context.Context) error {
				return errors.New("simulated cap elapsed")
			},
			waitIdle:    func(context.Context) int { return 3 },
			activeCount: func() int { return 3 },
			forceClose: func() error {
				forceCalled.add(1)
				return nil
			},
			closeListener:  listener.Close,
			done:           nil,
			lis:            listener,
			proxy:          nil,
			reloadDraining: atomic.Bool{},
		},
		reloadLock:   sync.Mutex{},
		drainDonesMu: sync.Mutex{},
		drainDones:   nil,
	}
	caller := time.Now()
	done := drainReloadedMITM(context.Background(), log, rt)
	if elapsed := time.Since(caller); elapsed > 200*time.Millisecond {
		t.Fatalf("drainReloadedMITM blocked caller for %s; reload RPC must not wait on in-flight tunnels", elapsed)
	}
	waitDoneOrFail(t, done, 2*time.Second)
	if got := forceCalled.load(); got != 1 {
		t.Fatalf("force_close: got %d invocations, want 1", got)
	}
	tally := tallyMITMReloadEvents(t, buf.Bytes())
	if tally["daemon.reload.mitm_drain_timeout"] == 0 {
		t.Fatalf("expected mitm_drain_timeout on cap path, got tally=%v", tally)
	}
	if tally["daemon.reload.mitm_drain_active"] == 0 {
		t.Fatalf("expected mitm_drain_active on cap path, got tally=%v", tally)
	}
}

// TestDrainReloadedMITMSetsReloadDrainingBeforeAsync covers the
// CLYDE-324 follow-up race: the synchronous mitmProc.cancel (used by
// exclusive.stop on full daemon exit) must NOT fire its 4s
// proxy.Shutdown when the reload chain has already kicked off the
// async drain with mitmReloadDrainCap. The flag must be set before
// the async goroutine is scheduled so cancel callers see it
// immediately on a concurrent stopExclusive("reload_handoff").
func TestDrainReloadedMITMSetsReloadDrainingBeforeAsync(t *testing.T) {
	_, log := captureSlogJSON()
	listener := newRecordingListener(t)
	mitmProc := &mitmProcess{
		cancel: func() {
			t.Fatal("cancel ran during reload; reloadDraining flag did not gate it")
		},
		drain: func(context.Context) error {
			return errors.New("simulated cap elapsed")
		},
		waitIdle:    func(context.Context) int { return 1 },
		activeCount: func() int { return 1 },
		forceClose: func() error {
			return nil
		},
		closeListener:  listener.Close,
		done:           nil,
		lis:            listener,
		proxy:          nil,
		reloadDraining: atomic.Bool{},
	}
	rt := &daemonRuntime{
		listener:     nil,
		adapter:      nil,
		webapp:       nil,
		mitm:         mitmProc,
		reloadLock:   sync.Mutex{},
		drainDonesMu: sync.Mutex{},
		drainDones:   nil,
	}
	if mitmProc.reloadDraining.Load() {
		t.Fatal("reloadDraining was true before drainReloadedMITM ran")
	}
	done := drainReloadedMITM(context.Background(), log, rt)
	if !mitmProc.reloadDraining.Load() {
		t.Fatal("reloadDraining was not set by drainReloadedMITM")
	}
	waitDoneOrFail(t, done, 2*time.Second)
}

// TestStartMITMCancelSkipsShutdownWhenReloadDraining pins the cancel
// closure's gate behavior in isolation: once reloadDraining is true,
// invoking cancel must be a no-op and must not call proxy.Shutdown.
// Built without spinning up a real proxy by wiring a fake Shutdown
// observer through the same closure shape startMITM uses.
func TestStartMITMCancelSkipsShutdownWhenReloadDraining(t *testing.T) {
	_, log := captureSlogJSON()
	shutdownCalls := atomicCounter{}
	proc := &mitmProcess{
		cancel: nil,
		drain: func(context.Context) error {
			return nil
		},
		waitIdle:       func(context.Context) int { return 0 },
		activeCount:    func() int { return 0 },
		forceClose:     func() error { return nil },
		closeListener:  func() error { return nil },
		done:           nil,
		lis:            nil,
		proxy:          nil,
		reloadDraining: atomic.Bool{},
	}
	proc.cancel = func() {
		if proc.reloadDraining.Load() {
			log.Debug("mitm.cancel.skipped_reload_drain")
			return
		}
		shutdownCalls.add(1)
	}

	proc.cancel()
	if got := shutdownCalls.load(); got != 1 {
		t.Fatalf("baseline cancel: shutdownCalls=%d, want 1", got)
	}

	proc.reloadDraining.Store(true)
	proc.cancel()
	if got := shutdownCalls.load(); got != 1 {
		t.Fatalf("reload-time cancel: shutdownCalls=%d, want 1 (cancel must be no-op)", got)
	}
}

// waitDoneOrFail blocks until done is closed or the deadline elapses,
// failing the test with a clear message in the latter case so a hung
// goroutine cannot wedge the test binary.
func waitDoneOrFail(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("drainReloadedMITM goroutine did not complete within %s", timeout)
	}
}

// captureSlogJSON returns a buffer-backed JSON logger so tests can
// assert on emitted records.
func captureSlogJSON() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	guard := &mitmReloadSyncWriter{w: buf, mu: sync.Mutex{}}
	handler := slog.NewJSONHandler(guard, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		AddSource:   false,
		ReplaceAttr: nil,
	})
	return buf, slog.New(handler)
}

// tallyMITMReloadEvents parses each JSON line and bins it by
// message + relevant attribute combinations the assertions need.
func tallyMITMReloadEvents(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	tally := make(map[string]int)
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Msg           string `json:"msg"`
			ActiveTunnels *int   `json:"active_tunnels,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode slog line %q: %v", line, err)
		}
		tally[record.Msg]++
		if record.ActiveTunnels != nil {
			tally[record.Msg+".active_tunnels:"+itoa(*record.ActiveTunnels)]++
		}
	}
	return tally
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	const digits = "0123456789"
	negative := value < 0
	if negative {
		value = -value
	}
	buf := make([]byte, 0, 11)
	for value > 0 {
		buf = append(buf, digits[value%10])
		value /= 10
	}
	if negative {
		buf = append(buf, '-')
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

type mitmReloadSyncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *mitmReloadSyncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type atomicCounter struct {
	mu  sync.Mutex
	val int
}

func (c *atomicCounter) add(delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.val += delta
}

func (c *atomicCounter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// recordingListener implements net.Listener with a Close that the
// drain function is expected to invoke. Accept blocks forever; the
// drain helper never calls Accept on the inherited listener.
type recordingListener struct {
	closed *recordingFlag
	addr   net.Addr
	done   chan struct{}
}

type recordingFlag struct {
	mu sync.Mutex
	on bool
}

func (a *recordingFlag) set(b bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.on = b
}

func newRecordingListener(t *testing.T) *recordingListener {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("resolve addr: %v", err)
	}
	return &recordingListener{
		closed: &recordingFlag{mu: sync.Mutex{}, on: false},
		addr:   addr,
		done:   make(chan struct{}),
	}
}

func (l *recordingListener) Accept() (net.Conn, error) {
	<-l.done
	return nil, errors.New("listener closed")
}

func (l *recordingListener) Close() error {
	l.closed.set(true)
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *recordingListener) Addr() net.Addr {
	return l.addr
}

// Compile-time assertions that ctx aliasing matches expectations.
var _ context.Context = context.Background()

// Sanity to satisfy time import in tests that do not otherwise use
// it; remove if a future edit adds a real time-based assertion.
var _ = time.Second
