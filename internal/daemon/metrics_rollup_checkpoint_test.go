package daemon_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

const (
	rollupCheckpointFileName = "metrics-rollup-checkpoint.json"
	rollupPassCompleted      = `"daemon.metrics_rollup.pass_completed"`
	rollupPassTimeout        = 30 * time.Second
)

type rollupLogRecord struct {
	Time      string `json:"time"`
	Message   string `json:"msg"`
	RequestID string `json:"request_id,omitempty"`
}

type rollupPassLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *rollupPassLog) Write(content []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(content)
}

func (l *rollupPassLog) completedPasses() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buffer.String(), rollupPassCompleted)
}

func appendDaemonLog(t *testing.T, path string, records ...rollupLogRecord) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func runRollupPass(t *testing.T, passes *rollupPassLog) {
	t.Helper()
	before := passes.completedPasses()
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	logger := slog.New(slog.NewJSONHandler(passes, &slog.HandlerOptions{AddSource: false, Level: slog.LevelDebug, ReplaceAttr: nil}))
	if !daemon.StartMetricsRollup(t.Context(), logger, group) {
		t.Fatal("metrics rollup did not start")
	}
	deadline := time.Now().Add(rollupPassTimeout)
	for passes.completedPasses() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	group.Quiesce(t.Context(), "test.stop", livetrack.Budget{Cap: rollupPassTimeout, IdleGrace: 0})
	if passes.completedPasses() != before+1 {
		t.Fatalf("metrics rollup completed %d passes, want one", passes.completedPasses()-before)
	}
}

func readRollupCheckpointFile(t *testing.T, path string) (os.FileInfo, []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return info, content
}

func TestMetricsRollupSkipsCheckpointWriteForUnchangedLog(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	logPath := slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
	if !strings.HasPrefix(logPath, stateHome) {
		t.Fatalf("daemon log path %s is outside the test state directory", logPath)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(config.DefaultStateDir(), rollupCheckpointFileName)
	passes := &rollupPassLog{mu: sync.Mutex{}, buffer: bytes.Buffer{}}

	appendDaemonLog(t, logPath, rollupLogRecord{Time: time.Now().UTC().Format(time.RFC3339Nano), Message: "daemon.health", RequestID: ""})
	runRollupPass(t, passes)
	requestAt := time.Now().UTC().Add(-10 * time.Minute)
	appendDaemonLog(t, logPath,
		rollupLogRecord{Time: requestAt.Format(time.RFC3339Nano), Message: "adapter.request.started", RequestID: "req-1"},
		rollupLogRecord{Time: requestAt.Add(time.Second).Format(time.RFC3339Nano), Message: "adapter.request.completed", RequestID: "req-1"},
	)
	runRollupPass(t, passes)

	infoBefore, contentBefore := readRollupCheckpointFile(t, checkpointPath)
	runRollupPass(t, passes)
	runRollupPass(t, passes)
	infoAfter, contentAfter := readRollupCheckpointFile(t, checkpointPath)
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) || !os.SameFile(infoBefore, infoAfter) || !bytes.Equal(contentBefore, contentAfter) {
		t.Fatalf("passes over an unchanged log rewrote the checkpoint: %s -> %s", contentBefore, contentAfter)
	}

	reportAt := time.Now().Add(30 * time.Minute)
	hourWindowFound := false
	for _, window := range daemon.MetricsWindowsFromRollup(reportAt) {
		if window.Label != "1h" {
			continue
		}
		hourWindowFound = true
		requests := window.Report.Metrics.Requests.Delta
		if requests == nil || *requests != 1 {
			t.Fatalf("hour window requests = %v, want 1", requests)
		}
		for _, warning := range window.Report.Warnings {
			if strings.Contains(warning, "may be up to") || strings.Contains(warning, "has not completed a pass") {
				t.Fatalf("caught-up store reported a stale last pass: %q", warning)
			}
		}
	}
	if !hourWindowFound {
		t.Fatal("metrics report has no hour window")
	}

	appendDaemonLog(t, logPath, rollupLogRecord{Time: time.Now().UTC().Format(time.RFC3339Nano), Message: "daemon.health", RequestID: ""})
	runRollupPass(t, passes)
	if _, contentChanged := readRollupCheckpointFile(t, checkpointPath); bytes.Equal(contentBefore, contentChanged) {
		t.Fatalf("pass over a changed log did not write the checkpoint: %s", contentChanged)
	}
}
