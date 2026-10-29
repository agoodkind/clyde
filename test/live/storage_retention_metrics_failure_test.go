//go:build live

package live

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveRetentionWaitsForMetricsCheckpointRecovery(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, h.cfg.MITMPort, []string{"all"})
	stateDirectory := filepath.Join(h.stateRoot, "clyde")
	checkpointPath := filepath.Join(stateDirectory, "metrics-rollup-checkpoint.json")
	if err := os.MkdirAll(checkpointPath, 0o700); err != nil {
		t.Fatal(err)
	}
	h.boot(t)
	paths := writeLiveRetentionLogs(t, h)
	original, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	processPath := filepath.Join(stateDirectory, "clyde-daemon.jsonl")
	waitLiveMetricsCleanupFailures(t, processPath, 2)
	for _, path := range paths {
		retained, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(retained, original) {
			t.Fatal("retention changed logs before saving the metrics checkpoint")
		}
	}
	if err := os.Remove(checkpointPath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(liveRetentionTimeout)
	for {
		retainedBytes, err := liveRetentionLogBytes(h.stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := os.Stat(checkpointPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil && checkpoint.Mode().IsRegular() && retainedBytes <= liveLogRetentionBudget+liveLogRetentionAllowance {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention did not resume after checkpoint recovery; retained bytes = %d", retainedBytes)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.assertProductionUntouched(t)
}

func waitLiveMetricsCleanupFailures(t *testing.T, path string, minimum int) {
	t.Helper()
	deadline := time.Now().Add(liveRetentionTimeout)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), `"msg":"daemon.log_cleanup.failed"`) >= minimum {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("retention did not report metrics checkpoint failures")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
