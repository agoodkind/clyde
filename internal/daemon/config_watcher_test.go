package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

// errTestParse is an arbitrary non-nil parse error injected in watcher tests.
var errTestParse = errors.New("test parse error")

// triggerRecorder records reload and rebind calls a test watcher makes.
type triggerRecorder struct {
	mu      sync.Mutex
	reloads int
	rebinds int
}

func (r *triggerRecorder) reload(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads++
	return nil
}

func (r *triggerRecorder) rebind(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rebinds++
	return nil
}

func (r *triggerRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloads, r.rebinds
}

// newTestConfigWatcher builds a watcher over a tempdir config path with a short
// debounce and injected parse, listenerChanged, reload, and rebind so the test
// needs no live daemon. baselineHash is the hash of the file's initial content.
func newTestConfigWatcher(t *testing.T, path, baselineHash string) (*configWatcher, *triggerRecorder, *atomic.Bool) {
	t.Helper()
	rec := &triggerRecorder{}
	var parseErr atomic.Pointer[error]
	listenerChanged := &atomic.Bool{}
	w := &configWatcher{
		path:         path,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		debounce:     15 * time.Millisecond,
		baselineHash: baselineHash,
		registry: livetrack.Attach[configWatcherMeta](livetrack.NewGroup(livetrack.GroupOptions{Log: nil}), livetrack.MemberSpec{
			Phase:         livetrack.PhaseWorkers,
			QuietRelevant: false,
			CancelNoWait:  true,
		}, livetrack.Options[configWatcherMeta]{
			Component:   "daemon",
			Concern:     slogger.ConcernProcessDaemonConfig,
			Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			PollEvery:   5 * time.Millisecond,
			CloserGrace: 200 * time.Millisecond,
		}),
		parse: func() (*config.Config, error) {
			if p := parseErr.Load(); p != nil {
				return nil, *p
			}
			return config.NewConfigWithDefaults(), nil
		},
		listenerChanged: listenerChanged.Load,
		reload:          rec.reload,
		rebind:          rec.rebind,
	}
	// expose parseErr through a closure stored on the watcher via a field would
	// require a struct change; instead the caller mutates via the returned
	// helpers. Store the pointer on a package-private map keyed by path.
	testParseErr[path] = &parseErr
	t.Cleanup(func() { delete(testParseErr, path) })
	return w, rec, listenerChanged
}

// testParseErr lets a test inject a parse error for a watcher keyed by its
// config path, without adding a test-only field to configWatcher.
var testParseErr = map[string]*atomic.Pointer[error]{}

func setParseErr(path string, err error) {
	if p, ok := testParseErr[path]; ok {
		if err == nil {
			p.Store(nil)
			return
		}
		p.Store(&err)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// TestHandleChangeWaitsForQuietThenTriggers asserts handleChange calls the
// quiet wait before triggering the reload, so a reload defers to in-flight
// client exchanges instead of severing them.
func TestHandleChangeWaitsForQuietThenTriggers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)

	// The changed content makes the post-wait hash differ from baseline.
	writeConfig(t, path, "# changed\n")

	quietReleased := make(chan struct{})
	waitEntered := make(chan struct{})
	w.quietWait = func(context.Context) bool {
		close(waitEntered)
		<-quietReleased
		return true
	}

	handleDone := make(chan bool, 1)
	go func() { handleDone <- w.handleChange(context.Background()) }()

	<-waitEntered
	if reloads, _ := rec.counts(); reloads != 0 {
		t.Fatalf("reload fired before quiet: reloads=%d, want 0", reloads)
	}
	close(quietReleased)

	select {
	case triggered := <-handleDone:
		if !triggered {
			t.Fatal("handleChange returned false; expected reload to trigger after quiet")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleChange did not return after quiet released")
	}
	if reloads, _ := rec.counts(); reloads != 1 {
		t.Fatalf("reloads after quiet = %d, want 1", reloads)
	}
}

// TestHandleChangeSkipsReloadWhenRevertedDuringWait asserts that if the file
// content returns to the baseline while waiting for quiet, no reload fires.
func TestHandleChangeSkipsReloadWhenRevertedDuringWait(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)

	// Change the content so handleChange's first hash differs from baseline.
	writeConfig(t, path, "# changed\n")

	// While "waiting for quiet", the operator reverts the edit back to baseline.
	w.quietWait = func(context.Context) bool {
		writeConfig(t, path, "# baseline\n")
		return true
	}

	if triggered := w.handleChange(context.Background()); triggered {
		t.Fatal("handleChange returned true; expected skip when reverted during wait")
	}
	if reloads, rebinds := rec.counts(); reloads != 0 || rebinds != 0 {
		t.Fatalf("triggers after revert: reloads=%d rebinds=%d, want 0 0", reloads, rebinds)
	}
}

// TestHandleChangeHotApplies asserts a hot-appliable edit calls applyInProcess,
// does not trigger a reload, updates the baseline hash, and returns false so the
// watcher keeps looping without a process replacement.
func TestHandleChangeHotApplies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	writeConfig(t, path, "# changed\n")
	changedHash := testConfigHash(t, path)

	applied := 0
	w.classify = func(*config.Config) config.Route { return config.RouteHotApply }
	w.applyInProcess = func(context.Context, *config.Config) error {
		applied++
		return nil
	}

	if triggered := w.handleChange(context.Background()); triggered {
		t.Fatal("handleChange returned true; hot apply must keep the loop running")
	}
	if applied != 1 {
		t.Fatalf("applyInProcess calls = %d, want 1", applied)
	}
	if reloads, rebinds := rec.counts(); reloads != 0 || rebinds != 0 {
		t.Fatalf("triggers on hot apply: reloads=%d rebinds=%d, want 0 0", reloads, rebinds)
	}
	if w.baselineHash != changedHash {
		t.Fatalf("baseline not advanced after hot apply: got %q, want %q", w.baselineHash, changedHash)
	}
}

// TestHandleChangeFallsBackToReloadOnApplyFailure asserts that when
// applyInProcess errors, the watcher proceeds to the quiet-wait reload path.
func TestHandleChangeFallsBackToReloadOnApplyFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	writeConfig(t, path, "# changed\n")

	w.classify = func(*config.Config) config.Route { return config.RouteHotApply }
	w.applyInProcess = func(context.Context, *config.Config) error {
		return errTestApplyFailed
	}

	if triggered := w.handleChange(context.Background()); !triggered {
		t.Fatal("handleChange returned false; expected reload fallback after apply failure")
	}
	if reloads, _ := rec.counts(); reloads != 1 {
		t.Fatalf("reloads after apply failure = %d, want 1", reloads)
	}
}

var errTestApplyFailed = errTestApply{}

type errTestApply struct{}

func (errTestApply) Error() string { return "test apply failed" }

// TestHandleChangeRestartRequiredSkips asserts a restart-required change neither
// hot-applies nor reloads; the operator must restart.
func TestHandleChangeRestartRequiredSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	writeConfig(t, path, "# changed\n")

	w.classify = func(*config.Config) config.Route { return config.RouteRestartRequired }

	if triggered := w.handleChange(context.Background()); triggered {
		t.Fatal("handleChange returned true; restart-required must not re-exec")
	}
	if reloads, rebinds := rec.counts(); reloads != 0 || rebinds != 0 {
		t.Fatalf("triggers on restart-required: reloads=%d rebinds=%d, want 0 0", reloads, rebinds)
	}
}

// testConfigHash computes the watcher's content hash for path with a discard
// logger, failing the test on a read error.
func testConfigHash(t *testing.T, path string) string {
	t.Helper()
	h, ok := configFileHash(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), path)
	if !ok {
		t.Fatalf("hash %s: read failed", path)
	}
	return h
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestConfigWatcherReloadsOnContentChange confirms one edit produces exactly
// one reload and no rebind.
func TestConfigWatcherReloadsOnContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	writeConfig(t, path, "# edited\n")
	waitFor(t, func() bool { r, _ := rec.counts(); return r == 1 }, "one reload")
	time.Sleep(60 * time.Millisecond)
	reloads, rebinds := rec.counts()
	if reloads != 1 || rebinds != 0 {
		t.Fatalf("reloads=%d rebinds=%d, want 1 and 0", reloads, rebinds)
	}
}

// TestConfigWatcherIgnoresBackupFile confirms a sibling config.toml.bak.* file
// appearing in the directory never triggers.
func TestConfigWatcherIgnoresBackupFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	writeConfig(t, filepath.Join(dir, config.ConfigFile+".bak.123"), "# backup\n")
	time.Sleep(100 * time.Millisecond)
	if reloads, rebinds := rec.counts(); reloads != 0 || rebinds != 0 {
		t.Fatalf("backup file triggered reloads=%d rebinds=%d, want 0 and 0", reloads, rebinds)
	}
}

// TestConfigWatcherSkipsBaselineContent confirms a touch that does not change
// the content (hash equals baseline) never triggers.
func TestConfigWatcherSkipsBaselineContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Rewrite identical content (a save with no change). Chmod also fires.
	writeConfig(t, path, "# baseline\n")
	_ = os.Chtimes(path, time.Now(), time.Now())
	time.Sleep(100 * time.Millisecond)
	if reloads, rebinds := rec.counts(); reloads != 0 || rebinds != 0 {
		t.Fatalf("baseline content triggered reloads=%d rebinds=%d, want 0 and 0", reloads, rebinds)
	}
}

// TestConfigWatcherParseFailureThenFix confirms broken content does not trigger
// and a following valid edit does.
func TestConfigWatcherParseFailureThenFix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, _ := newTestConfigWatcher(t, path, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	setParseErr(path, errTestParse) // any non-nil parse error
	writeConfig(t, path, "# broken\n")
	time.Sleep(100 * time.Millisecond)
	if reloads, _ := rec.counts(); reloads != 0 {
		t.Fatalf("parse failure triggered %d reloads, want 0", reloads)
	}

	setParseErr(path, nil)
	writeConfig(t, path, "# fixed\n")
	waitFor(t, func() bool { r, _ := rec.counts(); return r == 1 }, "one reload after fix")
}

// TestConfigWatcherPortChangeRoutesToRebind confirms a listener change routes to
// rebind, not reload.
func TestConfigWatcherPortChangeRoutesToRebind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, rec, listenerChanged := newTestConfigWatcher(t, path, baseline)
	listenerChanged.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	writeConfig(t, path, "# port changed\n")
	waitFor(t, func() bool { _, rb := rec.counts(); return rb == 1 }, "one rebind")
	if reloads, _ := rec.counts(); reloads != 0 {
		t.Fatalf("port change triggered %d reloads, want 0", reloads)
	}
}

// TestConfigWatcherCancelExitsLoop confirms cancelling the watcher loop exits it
// and releases its livetrack session. The group owns draining the registry on
// reload and shutdown; the watcher only cancels its own loop (non-blocking) so a
// reload it triggered cannot deadlock on its own drain.
func TestConfigWatcherCancelExitsLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ConfigFile)
	writeConfig(t, path, "# baseline\n")
	baseline := testConfigHash(t, path)
	w, _, _ := newTestConfigWatcher(t, path, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	w.cancelLoop()
	waitFor(t, func() bool { return w.registry.Count() == 0 }, "watcher session released")
}
