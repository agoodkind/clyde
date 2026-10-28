//go:build live

package live

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type unlinkedCleanupReport struct {
	Message           string    `json:"msg"`
	Time              time.Time `json:"time"`
	NamedBytesBefore  int64     `json:"bytes_before"`
	NamedBytesAfter   int64     `json:"bytes_after"`
	RetainedOpenBytes int64     `json:"retained_open_bytes"`
	RetainedOpenFiles int       `json:"retained_open_files"`
	ExcessBytes       int64     `json:"excess_bytes"`
	BytesTrimmed      int64     `json:"bytes_trimmed"`
}

func TestLiveStorageRetentionContinuesAfterSupervisorLogUnlink(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	h.daemonLog = filepath.Join(h.stateRoot, "supervisor-output.log")
	h.boot(t)

	probe := uniqueProbeID(t, "unlinked-retention")
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probe, true))
	first := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(first, "response.created") || strings.Contains(first, "response.completed") {
		t.Fatalf("held Responses stream did not return an initial frame; frame length = %d", len(first))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)

	line := []byte("{\"msg\":\"retained supervisor output\"}\n")
	data := bytes.Repeat(line, 6*1024*1024/len(line))
	if err := os.WriteFile(h.daemonLog, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(h.daemonLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.daemonLog); err != nil {
		t.Fatal(err)
	}
	paths := writeLiveRetentionLogs(t, h)
	report := waitForUnlinkedCleanup(t, h, stream, info.Size())
	if report.NamedBytesBefore <= liveLogRetentionBudget || report.BytesTrimmed <= 0 {
		t.Fatalf("cleanup did not prune named logs after supervisor unlink: %+v", report)
	}
	if report.NamedBytesAfter <= 0 || report.NamedBytesAfter > liveLogRetentionBudget {
		t.Fatalf("cleanup did not preserve bounded named logs: %+v", report)
	}
	if report.ExcessBytes != report.NamedBytesAfter+report.RetainedOpenBytes-liveLogRetentionBudget {
		t.Fatalf("cleanup did not report the physical excess: %+v", report)
	}
	for _, path := range paths {
		assertLiveRetentionJSONL(t, path)
	}
	assertUnlinkedReportPersists(t, h, stream, report)
	upstream.releaseStreams()
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") || !strings.Contains(completed, probe) {
		t.Fatalf("Responses stream did not complete after unlinked-log retention; response length = %d", len(completed))
	}
	h.assertProductionUntouched(t)
	t.Logf("named bytes = %d, retained open bytes = %d, retained open files = %d, unavoidable excess = %d", report.NamedBytesAfter, report.RetainedOpenBytes, report.RetainedOpenFiles, report.ExcessBytes)
}

func waitForUnlinkedCleanup(t *testing.T, h *harness, stream *responsesStream, expectedBytes int64) unlinkedCleanupReport {
	t.Helper()
	deadline := time.Now().Add(liveRetentionTimeout)
	for {
		assertUnlinkedStreamOpen(t, stream)
		reports := readUnlinkedCleanupReports(t, h.stateRoot)
		for _, report := range reports {
			if report.RetainedOpenFiles == 1 && report.RetainedOpenBytes == expectedBytes && report.BytesTrimmed > 0 {
				return report
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not prune logs while reporting %d unlinked bytes; reports = %+v", expectedBytes, reports)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertUnlinkedReportPersists(t *testing.T, h *harness, stream *responsesStream, initial unlinkedCleanupReport) {
	t.Helper()
	deadline := time.Now().Add(liveRetentionTimeout)
	for {
		assertUnlinkedStreamOpen(t, stream)
		for _, report := range readUnlinkedCleanupReports(t, h.stateRoot) {
			if report.Time.After(initial.Time) && report.RetainedOpenFiles == 1 && report.RetainedOpenBytes == initial.RetainedOpenBytes && report.ExcessBytes > 0 {
				if report.NamedBytesAfter <= 0 || report.NamedBytesAfter > liveLogRetentionBudget {
					t.Fatalf("later cleanup did not preserve bounded diagnostics while the unlinked floor exceeded the budget: %+v", report)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not continue reporting bytes retained by the open unlinked supervisor file")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertUnlinkedStreamOpen(t *testing.T, stream *responsesStream) {
	t.Helper()
	select {
	case body := <-stream.done:
		t.Fatalf("Responses stream ended during unlinked-log retention; response length = %d", len(body))
	default:
	}
}

func readUnlinkedCleanupReports(t *testing.T, root string) []unlinkedCleanupReport {
	t.Helper()
	var reports []unlinkedCleanupReport
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if !bytes.Contains(scanner.Bytes(), []byte("slogger.cleanup.completed")) {
				continue
			}
			var report unlinkedCleanupReport
			if err := json.Unmarshal(scanner.Bytes(), &report); err != nil {
				return err
			}
			if report.Message == "slogger.cleanup.completed" {
				reports = append(reports, report)
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return reports
}
