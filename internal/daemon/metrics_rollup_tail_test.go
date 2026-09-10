package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRollupState(t *testing.T, path string) *metricsRollupState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &metricsRollupState{}
	state.checkpoint.Source = rollupSourcePosition(path, info)
	state.checkpoint.Source.Offset = 0
	state.checkpoint.CoverageSince = "2026-08-01T00:00:00Z"
	t.Cleanup(func() {
		if err := state.closeSource(); err != nil {
			t.Error(err)
		}
	})
	return state
}

func appendRollupLog(t *testing.T, path string, records ...metricsHistoryRecord) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDistillRetainsRequestsAcrossPassesAndRotation(t *testing.T) {
	for _, rotation := range []string{"none", "rename", "compressed"} {
		t.Run(rotation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
			started := metricsHistoryRecord{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "shared", ExecutionID: " execution-1 "}
			streamed := metricsHistoryRecord{Time: "2026-08-08T10:00:01Z", Message: "adapter.request.stream_opened", RequestID: "shared", ExecutionID: "execution-1"}
			terminal := metricsHistoryRecord{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.completed", RequestID: "shared", ExecutionID: "execution-1", PromptTokens: 10, CompletionTokens: 20}
			writeMetricsHistoryRecords(t, path, []metricsHistoryRecord{started})
			input := metricsRollupDistillInput{LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), Now: rollupTestTime(t, "2026-08-08T11:00:00Z"), Pricing: rollupPricingTable(), State: testRollupState(t, path)}
			first, err := distillMetricsRollup(t.Context(), input)
			if err != nil || first.Written != 0 {
				t.Fatalf("unfinished first pass: %+v, %v", first, err)
			}
			appendRollupLog(t, path, streamed)
			if rotation != "none" {
				rotated := filepath.Join(filepath.Dir(path), "clyde-daemon-2026-08-08T10-00-01.000.jsonl")
				if err := os.Rename(path, rotated); err != nil {
					t.Fatal(err)
				}
				if rotation == "compressed" {
					writeGzipMetricsHistoryRecords(t, rotated+".gz", []metricsHistoryRecord{started, streamed})
					if err := os.Remove(rotated); err != nil {
						t.Fatal(err)
					}
				}
				writeMetricsHistoryRecords(t, path, nil)
			}
			appendRollupLog(t, path, terminal,
				metricsHistoryRecord{Time: terminal.Time, Message: "adapter.request.io", RequestID: "shared", ExecutionID: "execution-1", BytesIn: 5, BytesOut: 9},
				metricsHistoryRecord{Time: "2026-08-08T10:01:00Z", Message: "adapter.request.started", RequestID: "shared", ExecutionID: "execution-2"},
				metricsHistoryRecord{Time: "2026-08-08T10:01:01Z", Message: "adapter.request.failed", RequestID: "shared", ExecutionID: "execution-2"},
			)
			second, err := distillMetricsRollup(t.Context(), input)
			if err != nil || second.Written != 2 {
				t.Fatalf("completed second pass: %+v, %v", second, err)
			}
			direct := BuildMetricsHistory(MetricsHistoryInput{Since: input.Now.Add(-2 * time.Hour), Now: input.Now, LogPath: path, Pricing: input.Pricing})
			reports := metricsWindowsFromRollupPath(input.RollupPath, filepath.Join(t.TempDir(), "checkpoint"), []time.Duration{2 * time.Hour}, input.Now, input.Pricing)
			rolled := reports[0].Report
			if metricInt(direct.Metrics.Requests.Delta) != 2 {
				t.Fatalf("direct replay did not find both execution identities: %+v", direct)
			}
			assertCounterEqual(t, "requests", direct.Metrics.Requests, rolled.Metrics.Requests)
			assertCounterEqual(t, "failed", direct.Metrics.Failed, rolled.Metrics.Failed)
			assertCounterEqual(t, "input tokens", direct.Metrics.InputTokens, rolled.Metrics.InputTokens)
			assertCounterEqual(t, "output tokens", direct.Metrics.OutputTokens, rolled.Metrics.OutputTokens)
			assertCounterEqual(t, "bytes out", direct.Metrics.BytesOut, rolled.Metrics.BytesOut)
			if metricInt(direct.TimeBreakdown.Total.TotalMS) != metricInt(rolled.TimeBreakdown.Total.TotalMS) {
				t.Fatal("duration differs from direct replay")
			}
			third, err := distillMetricsRollup(t.Context(), input)
			if err != nil || third.BytesRead != 0 || third.Written != 0 {
				t.Fatalf("unchanged third pass: %+v, %v", third, err)
			}
		})
	}
}

func TestDistillLeavesIncompleteFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, path, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "daemon.health"},
		{Time: "2026-08-08T10:00:01Z", Message: "adapter.request.started", RequestID: "partial"},
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(metricsHistoryRecord{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.completed", RequestID: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(terminal); err != nil {
		t.Fatal(err)
	}
	input := metricsRollupDistillInput{LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), Now: rollupTestTime(t, "2026-08-08T11:00:00Z"), Pricing: rollupPricingTable(), State: testRollupState(t, path)}
	first, err := distillMetricsRollup(t.Context(), input)
	if err != nil || first.Written != 0 || input.State.checkpoint.Source.Offset != info.Size() {
		t.Fatalf("partial line was consumed: %+v offset=%d err=%v", first, input.State.checkpoint.Source.Offset, err)
	}
	if _, err := file.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil || second.Written != 1 || second.BytesRead != int64(len(terminal)+1) {
		t.Fatalf("completed line: %+v, %v", second, err)
	}
}

func TestDistillRetainsRecordsWrittenAfterPassStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, path, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "during-pass"},
		{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.completed", RequestID: "during-pass"},
	})
	input := metricsRollupDistillInput{LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), Now: rollupTestTime(t, "2026-08-08T10:00:01Z"), Pricing: rollupPricingTable(), State: testRollupState(t, path)}
	direct := BuildMetricsHistory(MetricsHistoryInput{Since: input.Now.Add(-time.Hour), Now: input.Now, LogPath: path, Pricing: input.Pricing})
	if metricInt(direct.Metrics.Requests.Delta) != 0 {
		t.Fatal("explicit historical replay included a future terminal")
	}
	first, err := distillMetricsRollup(t.Context(), input)
	if err != nil || first.Written != 0 {
		t.Fatalf("first pass: %+v %v", first, err)
	}
	input.Now = input.Now.Add(time.Minute)
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil || second.Written != 1 || second.BytesRead != 0 {
		t.Fatalf("next pass lost the already-read terminal: %+v %v", second, err)
	}
}

func TestDistillStartsAtEndWithoutCompatibleCheckpoint(t *testing.T) {
	for _, kind := range []string{"missing", "old", "replaced", "truncated", "compatible"} {
		t.Run(kind, func(t *testing.T) {
			path := twoRequestLog(t)
			state := testRollupState(t, path)
			switch kind {
			case "missing":
				state.checkpoint = metricsRollupCheckpoint{}
			case "old":
				state.checkpoint = metricsRollupCheckpoint{LastRecordAt: "2026-08-08T09:00:00Z"}
			case "replaced":
				state.checkpoint.Source.Inode++
			case "truncated":
				state.checkpoint.Source.Offset = 1 << 30
			}
			checkpointPath := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
			if err := writeMetricsRollupCheckpoint(checkpointPath, state.checkpoint); err != nil {
				t.Fatal(err)
			}
			state.checkpoint = readMetricsRollupCheckpoint(checkpointPath)
			rollupPath := writeRollupFixture(t, []metricsRollupRecord{requestRollupFixture("existing", "2026-08-08T08:30:00Z", "", "2026-08-08T08:30:01Z")})
			input := metricsRollupDistillInput{LogPath: path, RollupPath: rollupPath, Now: rollupTestTime(t, "2026-08-08T11:00:00Z"), Pricing: rollupPricingTable(), State: state}
			result, err := distillMetricsRollup(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "compatible" {
				want = 2
			} else if result.BytesRead != 0 {
				t.Fatalf("startup replayed %d historical bytes", result.BytesRead)
			}
			if result.Written != want {
				t.Fatalf("startup wrote %d, want %d", result.Written, want)
			}
			state.checkpoint.LastPassAt = formatRollupTime(input.Now)
			if err := writeMetricsRollupCheckpoint(checkpointPath, state.checkpoint); err != nil {
				t.Fatal(err)
			}
			reports := metricsWindowsFromRollupPath(rollupPath, checkpointPath, []time.Duration{3 * time.Hour}, input.Now, input.Pricing)
			if got := metricInt(reports[0].Report.Metrics.Requests.Delta); got != int64(want+1) {
				t.Fatalf("existing summary unreadable: requests=%d", got)
			}
			if kind != "compatible" && (reports[0].Report.Coverage.Complete || !hasWarningContaining(reports[0].Report, "historical summary coverage is unavailable")) {
				t.Fatalf("missing coverage unreported: %+v", reports[0].Report)
			}
		})
	}
}

func TestDistillResumesSavedCompleteOffset(t *testing.T) {
	path := twoRequestLog(t)
	input := metricsRollupDistillInput{LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), Now: rollupTestTime(t, "2026-08-08T11:00:00Z"), Pricing: rollupPricingTable(), State: testRollupState(t, path)}
	first, err := distillMetricsRollup(t.Context(), input)
	if err != nil || first.Written != 2 {
		t.Fatalf("first pass: %+v %v", first, err)
	}
	checkpointPath := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	if err := writeMetricsRollupCheckpoint(checkpointPath, input.State.checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := input.State.closeSource(); err != nil {
		t.Fatal(err)
	}
	appendRollupLog(t, path,
		metricsHistoryRecord{Time: "2026-08-08T10:30:00Z", Message: "adapter.request.started", RequestID: "new"},
		metricsHistoryRecord{Time: "2026-08-08T10:30:01Z", Message: "adapter.request.completed", RequestID: "new"},
	)
	input.State = testRollupState(t, path)
	input.State.checkpoint = readMetricsRollupCheckpoint(checkpointPath)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := info.Size() - input.State.checkpoint.Source.Offset
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil || second.Written != 1 || second.BytesRead != wantBytes {
		t.Fatalf("resumed pass: %+v %v; want %d new bytes", second, err, wantBytes)
	}
	reports := metricsWindowsFromRollupPath(input.RollupPath, checkpointPath, []time.Duration{2 * time.Hour}, input.Now, input.Pricing)
	if metricInt(reports[0].Report.Metrics.Requests.Delta) != 3 {
		t.Fatal("resumed report lost or duplicated requests")
	}
}

func TestDistillPrunesOnlyWhenRetentionIsDue(t *testing.T) {
	path := twoRequestLog(t)
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	state := testRollupState(t, path)
	input := metricsRollupDistillInput{LogPath: path, RollupPath: rollupPath, Now: rollupTestTime(t, "2026-08-08T11:00:00Z"), Pricing: rollupPricingTable(), State: state}
	if _, err := distillMetricsRollup(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	temporary := rollupPath + ".tmp"
	marker := []byte("prune has not opened this path")
	if err := os.WriteFile(temporary, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	input.Now = state.nextExpiry
	result, err := distillMetricsRollup(t.Context(), input)
	if err != nil || result.Pruned != 0 {
		t.Fatalf("not due: %+v %v", result, err)
	}
	got, err := os.ReadFile(temporary)
	if err != nil || !bytes.Equal(got, marker) {
		t.Fatalf("not-due prune touched its temporary file: %q %v", got, err)
	}
	input.Now = input.Now.Add(time.Nanosecond)
	result, err = distillMetricsRollup(t.Context(), input)
	if err != nil || result.Pruned != 1 {
		t.Fatalf("due: %+v %v", result, err)
	}
	loaded, err := loadMetricsRollup(rollupPath, []MetricsWindow{{Since: time.Time{}, Until: input.Now}})
	if err != nil || len(loaded[0].Requests) != 1 || loaded[0].Requests["req-2"] == nil {
		t.Fatalf("retention removed wrong request: %+v %v", loaded, err)
	}
	if !state.nextExpiry.After(input.Now) {
		t.Fatal("next retained expiry was not remembered")
	}
}

func TestRollupWorkerClosesKnownSourceOnStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := twoRequestLog(t)
	state := testRollupState(t, path)
	if err := state.openSource(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	file := state.source
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	worker := metricsRollupWorker{logPath: path, rollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), interval: metricsRollupInterval, log: slog.Default(), now: time.Now, state: *state, pricing: rollupPricingTable()}
	worker.run(ctx)
	state.source = nil
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("worker left its source descriptor open")
	}
}
