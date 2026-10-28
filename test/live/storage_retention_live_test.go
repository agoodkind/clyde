//go:build live

package live

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const (
	liveLogRetentionBudget     = 5 * 1024 * 1024
	liveLogRetentionAllowance  = 256 * 1024
	liveCaptureRetentionBudget = 8 * 1024 * 1024
	liveRetentionTimeout       = 20 * time.Second
)

func TestLiveStorageRetentionPreservesResponsesStream(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	configureLiveCaptureRetention(t, h)
	h.boot(t)

	oldProbe := uniqueProbeID(t, "retention-history")
	response := h.postResponses(t, h.cfg.AdapterPort, responsesRequestBody(oldProbe, false))
	if response.statusCode != http.StatusOK {
		t.Fatalf("capture fixture status = %d, want %d; synthetic response = %s", response.statusCode, http.StatusOK, response.body)
	}
	_ = upstream.waitForRequest(t, 10*time.Second)
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, oldProbe, 10*time.Second), oldProbe, false)

	probe := uniqueProbeID(t, "retention-stream")
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probe, true))
	first := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(first, "response.created") || strings.Contains(first, "response.completed") {
		t.Fatalf("held Responses stream did not return an initial frame; frame length = %d", len(first))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)

	excluded := writeLiveRetentionExcludedFiles(t, h)
	paths := writeLiveRetentionLogs(t, h)
	logBytes, captureBytes := waitLiveRetentionDuringStream(t, h, stream, oldProbe)
	t.Logf("retained log bytes = %d, capture database/WAL/SHM bytes = %d", logBytes, captureBytes)
	for _, path := range paths {
		assertLiveRetentionJSONL(t, path)
	}
	for _, fixture := range excluded {
		data, err := os.ReadFile(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, fixture.data) {
			t.Errorf("retention changed an excluded private file: %s", fixture.path)
		}
	}
	upstream.releaseStreams()
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") || !strings.Contains(completed, probe) {
		t.Fatalf("Responses stream did not complete after retention; response length = %d", len(completed))
	}
	h.assertProductionUntouched(t)
}

type liveRetentionExcludedFile struct {
	path string
	data []byte
}

func writeLiveRetentionExcludedFiles(t *testing.T, h *harness) []liveRetentionExcludedFile {
	t.Helper()
	old := time.Now().Add(-14 * 24 * time.Hour)
	var fixtures []liveRetentionExcludedFile
	for _, directory := range []string{"desktop-via-clyde-backup", "search"} {
		root := filepath.Join(h.stateRoot, "clyde", directory)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "preserved.jsonl")
		data := []byte(fmt.Sprintf("{\"source\":%q,\"value\":\"preserve\"}\n", directory))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, liveRetentionExcludedFile{path: path, data: data})
	}
	return fixtures
}

func configureLiveCaptureRetention(t *testing.T, h *harness) {
	t.Helper()
	content, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	const section = "[mitm.capture_store]\n"
	settings := fmt.Sprintf("%sretention_max_age = \"3s\"\nretention_max_bytes = %d\nretention_interval = \"100ms\"\n", section, liveCaptureRetentionBudget)
	updated := strings.Replace(string(content), section, settings, 1)
	if updated == string(content) {
		t.Fatal("private config does not contain the capture store section")
	}
	if err := os.WriteFile(h.configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLiveRetentionLogs(t *testing.T, h *harness) []string {
	t.Helper()
	root := filepath.Join(h.stateRoot, "clyde", "logs", "retention-live")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	line := []byte("{\"msg\":\"retention fixture\",\"payload\":\"" + strings.Repeat("x", 16*1024) + "\"}\n")
	data := bytes.Repeat(line, 256)
	var paths []string
	var locks []*flock.Flock
	defer func() {
		for _, lock := range locks {
			if err := lock.Unlock(); err != nil {
				t.Error(err)
			}
		}
	}()
	for index := range 2 {
		path := filepath.Join(root, fmt.Sprintf("fixture-%d.jsonl", index))
		lock := flock.New(path + ".lock")
		if err := lock.Lock(); err != nil {
			t.Fatal(err)
		}
		locks = append(locks, lock)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if len(data)*len(paths) <= liveLogRetentionBudget {
		t.Fatal("private log fixtures do not exceed the combined budget")
	}
	return paths
}

func waitLiveRetentionDuringStream(t *testing.T, h *harness, stream *responsesStream, oldProbe string) (int64, int64) {
	t.Helper()
	deadline := time.Now().Add(liveRetentionTimeout)
	databasePath := filepath.Join(h.stateRoot, "mitm", "capture.db")
	var logBytes, captureBytes int64
	for {
		select {
		case body := <-stream.done:
			t.Fatalf("Responses stream ended during retention; response length = %d", len(body))
		default:
		}
		var err error
		logBytes, err = liveRetentionLogBytes(h.stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		captureBytes = liveRetentionCaptureBytes(t, databasePath)
		oldCaptures, err := readResponsesCaptures(databasePath, oldProbe)
		if err != nil {
			t.Fatal(err)
		}
		if logBytes <= liveLogRetentionBudget+liveLogRetentionAllowance && captureBytes <= liveCaptureRetentionBudget && len(oldCaptures) == 0 {
			return logBytes, captureBytes
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention did not finish during stream: log bytes = %d, capture bytes = %d, old capture rows = %d", logBytes, captureBytes, len(oldCaptures))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func liveRetentionLogBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".log") && !strings.HasSuffix(path, ".jsonl.gz") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func liveRetentionCaptureBytes(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

func assertLiveRetentionJSONL(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if !json.Valid(scanner.Bytes()) {
			t.Fatalf("retained log contains invalid JSON: %s", path)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
