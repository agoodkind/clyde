package daemon

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

func TestBuildMetricsHistoryDerivesExclusiveStageTime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "req-1"},
		{Time: "2026-07-31T10:00:00.030Z", Message: "adapter.request.stream_opened", RequestID: "req-1", Provider: "codex"},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "req-1", DurationMS: 100, Provider: "codex", PromptTokens: 4, CompletionTokens: 6},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.io", RequestID: "req-1", BytesIn: 10, BytesOut: 20},
	})

	report := BuildMetricsHistory(MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		LogPath: logPath,
	})
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = %+v, want complete", report.Coverage)
	}
	if metricInt(report.Metrics.Requests.Delta) != 1 {
		t.Fatalf("request delta = %v, want 1", report.Metrics.Requests.Delta)
	}
	if metricInt(report.Metrics.BytesIn.Delta) != 10 || metricInt(report.Metrics.BytesOut.Delta) != 20 {
		t.Fatalf("byte deltas = %+v, want 10 and 20", report.Metrics)
	}
	if metricInt(report.Metrics.InputTokens.Delta) != 4 || metricInt(report.Metrics.OutputTokens.Delta) != 6 {
		t.Fatalf("token deltas = %+v, want 4 and 6", report.Metrics)
	}
	if got := metricStage(t, report, "provider_to_first_response"); metricInt(got.TotalMS) != 30 {
		t.Fatalf("provider first response time = %+v, want total 30", got)
	}
	if got := metricStage(t, report, "response_streaming"); metricInt(got.TotalMS) != 70 {
		t.Fatalf("response streaming time = %+v, want total 70", got)
	}
	if metricInt(report.UnattributedDurationMS) != 0 {
		t.Fatalf("unattributed duration = %v, want 0", report.UnattributedDurationMS)
	}
}

func TestBuildMetricsHistoryDoesNotReadLMSSFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "req-clyde"},
		{Time: "2026-07-31T10:00:00.020Z", Message: "adapter.request.completed", RequestID: "req-clyde", DurationMS: 20},
	})
	poisonPath := filepath.Join(root, "lm-semantic-search", "state.jsonl")
	if err := os.MkdirAll(filepath.Dir(poisonPath), 0o755); err != nil {
		t.Fatalf("mkdir poison: %v", err)
	}
	if err := os.WriteFile(poisonPath, []byte("this must never be read\n"), 0o600); err != nil {
		t.Fatalf("write poison: %v", err)
	}

	report := BuildMetricsHistory(MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		LogPath: logPath,
	})
	if metricInt(report.Metrics.Requests.Delta) != 1 {
		t.Fatalf("request delta = %v, want 1", report.Metrics.Requests.Delta)
	}
	for _, warning := range report.Warnings {
		if warning == poisonPath {
			t.Fatalf("metrics read poison path %q", poisonPath)
		}
	}
}

func TestBuildMetricsHistoryIgnoresSameDirectoryPoisonFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "process.daemon.started"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "req-clyde"},
		{Time: "2026-07-31T10:00:00.020Z", Message: "adapter.request.completed", RequestID: "req-clyde", DurationMS: 20},
	})
	poisonNames := []string{
		"clyde-daemon.jsonl.lock",
		"clyde-daemon.jsonl-arbitrary",
		"clyde-daemon-2026-07-31T10-00-00.jsonl",
		"clyde-daemon-2026-07-31T10-00-00.000.jsonl.bad",
	}
	for _, name := range poisonNames {
		if err := os.WriteFile(filepath.Join(root, name), []byte("malformed\n"), 0o600); err != nil {
			t.Fatalf("write poison %s: %v", name, err)
		}
	}

	report := BuildMetricsHistory(MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		LogPath: logPath,
	})
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %q, want none", report.Warnings)
	}
}

func TestBuildMetricsHistoryEmptyHistoryIsIncomplete(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatalf("write empty log: %v", err)
	}
	report := BuildMetricsHistory(MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		LogPath: logPath,
	})
	if report.Coverage.Complete {
		t.Fatal("empty history coverage = complete, want incomplete")
	}
}

func TestBuildMetricsHistoryRetainedStartGapIsIncomplete(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{{Time: "2026-07-31T10:00:00Z", Message: "daemon.health"}})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Coverage.Complete {
		t.Fatal("coverage = complete, want retained start gap")
	}
	if len(report.Warnings) != 1 || report.Warnings[0] != "retained daemon history starts after the requested window" {
		t.Fatalf("warnings = %q, want retained history gap", report.Warnings)
	}
}

func TestBuildMetricsHistoryRetainsPredecessorForCrossingWindowRequest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, filepath.Join(root, "clyde-daemon-2026-07-31T08-59-59.500.jsonl"), []metricsHistoryRecord{
		{Time: "2026-07-31T08:59:59Z", Message: "adapter.request.started", RequestID: "crossing"},
	})
	if err := os.WriteFile(filepath.Join(root, "clyde-daemon-2026-07-31T07-00-00.000.jsonl.gz"), []byte("old corrupt gzip"), 0o600); err != nil {
		t.Fatalf("write old rotation: %v", err)
	}
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T09:00:01Z", Message: "adapter.request.completed", RequestID: "crossing"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = %+v warnings=%q, want complete", report.Coverage, report.Warnings)
	}
	if metricInt(report.Metrics.Requests.Delta) != 1 {
		t.Fatalf("requests delta = %v, want 1", report.Metrics.Requests.Delta)
	}
}

func TestBuildMetricsHistoryDerivesRotationsFromCustomActivePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "custom-daemon.jsonl")
	writeGzipMetricsHistoryRecords(t, filepath.Join(root, "custom-daemon-2026-07-31T08-59-59.500.jsonl.gz"), []metricsHistoryRecord{
		{Time: "2026-07-31T08:59:59Z", Message: "adapter.request.started", RequestID: "custom-crossing"},
	})
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T09:00:01Z", Message: "adapter.request.completed", RequestID: "custom-crossing"},
	})
	poisonNames := []string{
		"custom-daemon.jsonl.lock",
		"custom-daemon-2026-07-31T09-30-00.000.jsonl.bad",
		"clyde-daemon-2026-07-31T09-30-00.000.jsonl",
	}
	for _, name := range poisonNames {
		if err := os.WriteFile(filepath.Join(root, name), []byte("malformed\n"), 0o600); err != nil {
			t.Fatalf("write poison %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "custom-daemon-2026-07-31T07-00-00.000.jsonl.gz"), []byte("old corrupt gzip"), 0o600); err != nil {
		t.Fatalf("write old custom rotation: %v", err)
	}

	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete || metricInt(report.Metrics.Requests.Delta) != 1 || len(report.Warnings) != 0 {
		t.Fatalf("custom report = coverage %+v requests %v warnings %q", report.Coverage, report.Metrics.Requests.Delta, report.Warnings)
	}
}

func TestBuildMetricsHistoryRestartInvalidatesGenerationCounters(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T09:30:00Z", Message: "daemon.worker.ready"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, []ProviderStats{{Requests: 4}}, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Coverage.Complete || report.Metrics.Requests.Delta != nil || report.Metrics.Requests.RatePerSecond != nil {
		t.Fatalf("restart report = %+v, want incomplete null request history", report)
	}
	if report.Metrics.Requests.Current == nil || *report.Metrics.Requests.Current != 4 {
		t.Fatalf("current requests = %v, want 4", report.Metrics.Requests.Current)
	}
}

func TestApplyCurrentProviderSnapshotLoadedInsideWindowInvalidatesGenerationCounters(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"}})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, []ProviderStats{{Requests: 7, InputTokens: 8}}, time.Date(2026, 7, 31, 9, 30, 0, 0, time.UTC))
	if report.Coverage.Complete || report.Metrics.Requests.Delta != nil || report.Metrics.InputTokens.RatePerSecond != nil {
		t.Fatalf("generation report = %+v, want incomplete null history", report)
	}
	if metricInt(report.Metrics.Requests.Current) != 7 || metricInt(report.Metrics.InputTokens.Current) != 8 {
		t.Fatalf("live currents = %+v, want preserved", report.Metrics)
	}
}

func TestBuildMetricsHistoryMalformedRelevantTimestampIsIncomplete(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "bad", Message: "adapter.request.started", RequestID: "bad-time"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Coverage.Complete {
		t.Fatal("coverage = complete, want malformed relevant timestamp incomplete")
	}
}

func TestBuildMetricsHistoryIgnoresUnrelatedConflictingFieldTypes(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	body := []byte("{\"time\":\"2026-07-31T09:00:00Z\",\"msg\":\"daemon.health\",\"status\":200}\n" +
		"{\"time\":\"2026-07-31T10:00:00Z\",\"msg\":\"adapter.request.started\",\"request_id\":\"valid\"}\n" +
		"{\"time\":\"2026-07-31T10:00:00.100Z\",\"msg\":\"adapter.request.completed\",\"request_id\":\"valid\"}\n")
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("write history log: %v", err)
	}
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = incomplete warnings=%q, want complete", report.Warnings)
	}
	if metricInt(report.Metrics.Requests.Delta) != 1 {
		t.Fatalf("request delta = %v, want 1", report.Metrics.Requests.Delta)
	}
}

func TestBuildMetricsHistoryCorruptGzipIsIncomplete(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logPath := filepath.Join(root, "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"}})
	rotation := filepath.Join(root, "clyde-daemon-2026-07-31T09-30-00.000.jsonl.gz")
	if err := os.WriteFile(rotation, []byte("not gzip"), 0o600); err != nil {
		t.Fatalf("write corrupt rotation: %v", err)
	}
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Coverage.Complete {
		t.Fatal("coverage = complete, want corrupt gzip incomplete")
	}
}

func TestBuildMetricsHistoryActiveRequestAtWindowEndIsNormal(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "active"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = incomplete warnings=%q, want complete", report.Warnings)
	}
}

func TestBuildMetricsHistoryTerminalWithoutStartIsIncomplete(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.completed", RequestID: "missing-start"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Coverage.Complete {
		t.Fatal("coverage = complete, want terminal without start incomplete")
	}
}

func TestBuildMetricsHistorySeparatesExecutionsSharingRequestID(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
		{Time: "2026-07-31T10:01:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-2"},
		{Time: "2026-07-31T10:01:00.100Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-2"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = incomplete warnings=%q, want complete", report.Warnings)
	}
	if metricInt(report.Metrics.Requests.Delta) != 2 {
		t.Fatalf("request delta = %v, want 2", report.Metrics.Requests.Delta)
	}
}

func TestBuildMetricsHistoryRejectsImpossibleLifecycle(t *testing.T) {
	t.Parallel()
	testCases := map[string]struct {
		records []metricsHistoryRecord
		warning string
	}{
		"terminal_before_start": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request terminal record precedes start",
		},
		"stream_before_start": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request stream record precedes start",
		},
		"stream_after_terminal": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.200Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request stream record follows terminal",
		},
		"start_timestamp_after_terminal": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00.200Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request start timestamp follows terminal timestamp",
		},
		"stream_timestamp_before_start": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:01Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request stream timestamp precedes start timestamp",
		},
		"terminal_timestamp_before_stream": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:02Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:01Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request terminal timestamp precedes stream timestamp",
		},
		"duplicate_streams": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.200Z", Message: "adapter.request.stream_opened", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.300Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request has duplicate stream records",
		},
		"duplicate_starts": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request has duplicate start records",
		},
		"duplicate_terminals": {
			records: []metricsHistoryRecord{
				{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "caller-id", ExecutionID: "exec-1"},
				{Time: "2026-07-31T10:00:00.200Z", Message: "adapter.request.failed", RequestID: "caller-id", ExecutionID: "exec-1"},
			},
			warning: "request has duplicate terminal records",
		},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
			records := append([]metricsHistoryRecord{{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"}}, testCase.records...)
			writeMetricsHistoryRecords(t, logPath, records)
			report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
			ApplyCurrentProviderSnapshot(&report, nil, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
			if report.Coverage.Complete {
				t.Fatal("coverage = complete, want incomplete")
			}
			if !slices.Contains(report.Warnings, testCase.warning) {
				t.Fatalf("warnings = %q, want %q", report.Warnings, testCase.warning)
			}
		})
	}
}

func TestBuildMetricsHistoryRanksStagesAndUsesAllPercentileSamples(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "one"},
		{Time: "2026-07-31T10:00:00.010Z", Message: "adapter.request.stream_opened", RequestID: "one"},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "one"},
		{Time: "2026-07-31T10:01:00Z", Message: "adapter.request.started", RequestID: "two"},
		{Time: "2026-07-31T10:01:00.030Z", Message: "adapter.request.stream_opened", RequestID: "two"},
		{Time: "2026-07-31T10:01:00.050Z", Message: "adapter.request.completed", RequestID: "two"},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	if len(report.TimeBreakdown.Stages) != 2 || report.TimeBreakdown.Stages[0].Name != "response_streaming" {
		t.Fatalf("stages = %+v, want response_streaming first", report.TimeBreakdown.Stages)
	}
	streaming := report.TimeBreakdown.Stages[0]
	if metricInt(streaming.TotalMS) != 110 || metricInt(streaming.P50MS) != 20 || metricInt(streaming.P95MS) != 90 {
		t.Fatalf("streaming duration = %+v, want total=110 p50=20 p95=90", streaming)
	}
}

func TestBuildMetricsHistoryPricesKnownModelsExactly(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "priced"},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "priced", Model: "known", PromptTokens: 4, CompletionTokens: 6, CacheReadTokens: 2, CacheCreationTokens: 3},
	})
	input := testMetricsHistoryInput(logPath)
	input.Pricing = adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"known": {InputPerMTok: 1, OutputPerMTok: 2, CacheReadPerMTok: 0.1, CacheWritePerMTok: 1.25},
	})
	report := BuildMetricsHistory(input)
	if metricInt(report.Metrics.CostMicrocents.Delta) != 1995 {
		t.Fatalf("cost delta = %v, want 1995", report.Metrics.CostMicrocents.Delta)
	}
}

func TestBuildMetricsHistoryUnknownModelMakesCostUnavailable(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-07-31T09:00:00Z", Message: "daemon.health"},
		{Time: "2026-07-31T10:00:00Z", Message: "adapter.request.started", RequestID: "unknown"},
		{Time: "2026-07-31T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "unknown", Model: "unknown", PromptTokens: 1},
	})
	report := BuildMetricsHistory(testMetricsHistoryInput(logPath))
	if report.Metrics.CostMicrocents.Delta != nil || report.Metrics.CostMicrocents.RatePerSecond != nil {
		t.Fatalf("unknown cost = %+v, want null", report.Metrics.CostMicrocents)
	}
	if len(report.Warnings) != 1 || report.Warnings[0] != "pricing unavailable for model unknown" {
		t.Fatalf("warnings = %q, want unknown pricing warning", report.Warnings)
	}
}

func TestApplyCurrentProviderSnapshotKeepsUnavailableCurrentValuesNil(t *testing.T) {
	t.Parallel()
	report := BuildMetricsHistory(MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		LogPath: filepath.Join(t.TempDir(), "clyde-daemon.jsonl"),
	})
	ApplyCurrentProviderSnapshot(&report, []ProviderStats{{
		ProviderDetail: "codex", Requests: 4, Inflight: 2, Streaming: 1,
		InputTokens: 40, OutputTokens: 60, CacheReadTokens: 10, CacheCreationTokens: 5,
		EstimatedCostMicrocents: 99, LastSeenUnix: 0, Error: "", DerivedCacheCreationTokens: 0,
	}}, time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC))
	if report.Metrics.Requests.Current == nil || *report.Metrics.Requests.Current != 4 {
		t.Fatalf("requests current = %v, want 4", report.Metrics.Requests.Current)
	}
	if report.Metrics.BytesIn.Current != nil || report.Metrics.Completed.Current != nil {
		t.Fatalf("unavailable live current values = bytes=%v completed=%v", report.Metrics.BytesIn.Current, report.Metrics.Completed.Current)
	}
}

type metricsHistoryRecord struct {
	Time                string `json:"time"`
	Message             string `json:"msg"`
	RequestID           string `json:"request_id"`
	ExecutionID         string `json:"execution_id,omitempty"`
	Leg                 string `json:"leg"`
	DurationMS          int64  `json:"duration_ms"`
	Status              string `json:"status"`
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	BytesIn             int64  `json:"bytes_in"`
	BytesOut            int64  `json:"bytes_out"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
}

func writeMetricsHistoryRecords(t *testing.T, path string, records []metricsHistoryRecord) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create history log: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("encode history record: %v", err)
		}
	}
}

func writeGzipMetricsHistoryRecords(t *testing.T, path string, records []metricsHistoryRecord) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create gzip history log: %v", err)
	}
	gzipWriter := gzip.NewWriter(file)
	encoder := json.NewEncoder(gzipWriter)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("encode gzip history record: %v", err)
		}
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip history writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close gzip history log: %v", err)
	}
}

func metricInt(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

func metricStage(t *testing.T, report MetricsHistoryReport, name string) MetricsStageDuration {
	t.Helper()
	for _, stage := range report.TimeBreakdown.Stages {
		if stage.Name == name {
			return stage
		}
	}
	t.Fatalf("stage %q missing", name)
	return MetricsStageDuration{}
}

func testMetricsHistoryInput(logPath string) MetricsHistoryInput {
	return MetricsHistoryInput{
		Since:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
		Now:     time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		LogPath: logPath,
	}
}
