package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rotationOnReadContext schedules rotation at the reader's existing context
// check, after its first content read. This fixes the interleaving without sleeps.
type rotationOnReadContext struct {
	context.Context
	source  *os.File
	rotate  func()
	rotated bool
}

func (ctx *rotationOnReadContext) Err() error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}
	if ctx.rotated {
		return nil
	}
	offset, err := ctx.source.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if offset > 0 {
		ctx.rotated = true
		ctx.rotate()
	}
	return nil
}

func TestDistillDrainsAppendAfterSnapshotBeforeRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, path, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "during-rotation"},
	})
	state := testRollupState(t, path)
	now := rollupTestTime(t, "2026-08-08T11:00:00Z")
	if err := state.openSource(path, now); err != nil {
		t.Fatal(err)
	}
	ctx := &rotationOnReadContext{
		Context: t.Context(), source: state.source,
		rotate: func() {
			appendRollupLog(t, path,
				metricsHistoryRecord{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.completed", RequestID: "during-rotation", PromptTokens: 10, CompletionTokens: 20},
				metricsHistoryRecord{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.io", RequestID: "during-rotation", BytesIn: 5, BytesOut: 9},
			)
			rotated := filepath.Join(filepath.Dir(path), "clyde-daemon-2026-08-08T10-00-03.000.jsonl")
			if err := os.Rename(path, rotated); err != nil {
				t.Fatal(err)
			}
			writeMetricsHistoryRecords(t, path, nil)
		},
	}
	input := metricsRollupDistillInput{LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName), Now: now, Pricing: rollupPricingTable(), State: state}
	first, err := distillMetricsRollup(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.rotated {
		t.Fatal("fixture did not rotate after reading began")
	}
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	direct := BuildMetricsHistory(MetricsHistoryInput{Since: now.Add(-2 * time.Hour), Now: now, LogPath: path, Pricing: input.Pricing})
	reports := metricsWindowsFromRollupPath(input.RollupPath, filepath.Join(t.TempDir(), "checkpoint"), []time.Duration{2 * time.Hour}, now, input.Pricing)
	rolled := reports[0].Report
	t.Logf("direct=%d first_written=%d second_written=%d pending=%d", metricInt(direct.Metrics.Requests.Delta), first.Written, second.Written, len(state.requests))
	if metricInt(direct.Metrics.Requests.Delta) != 1 {
		t.Fatal("direct replay did not find the completed request")
	}
	if first.Written != 1 || second.Written != 0 || len(state.requests) != 0 {
		t.Fatal("rotation lost the completion appended after the initial snapshot")
	}
	if second.BytesRead != 0 {
		t.Fatalf("unchanged next pass read %d bytes", second.BytesRead)
	}
	assertCounterEqual(t, "requests", direct.Metrics.Requests, rolled.Metrics.Requests)
	assertCounterEqual(t, "bytes in", direct.Metrics.BytesIn, rolled.Metrics.BytesIn)
	assertCounterEqual(t, "bytes out", direct.Metrics.BytesOut, rolled.Metrics.BytesOut)
	assertCounterEqual(t, "input tokens", direct.Metrics.InputTokens, rolled.Metrics.InputTokens)
	assertCounterEqual(t, "output tokens", direct.Metrics.OutputTokens, rolled.Metrics.OutputTokens)
}
