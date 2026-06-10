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
		registry: livetrack.New[configWatcherMeta](livetrack.Options[configWatcherMeta]{
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

// TestConfigWatcherStopExitsLoop confirms draining the watcher exits the loop.
func TestConfigWatcherStopExitsLoop(t *testing.T) {
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
	w.stop(context.Background(), "test")
	waitFor(t, func() bool { return w.registry.State() != livetrack.StateOpen }, "registry drained")
}
