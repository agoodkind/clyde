package daemon

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDistillKeepsUnfinishedLifecyclesAmongHealthRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, path, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "ongoing"},
		{Time: "2026-08-08T10:00:01Z", Message: "adapter.request.io", RequestID: "health-1", BytesOut: 16},
		{Time: "2026-08-08T10:00:02Z", Message: "adapter.request.started", RequestID: "finished"},
		{Time: "2026-08-08T10:00:03Z", Message: "adapter.request.completed", RequestID: "finished"},
		{Time: "2026-08-08T10:00:03Z", Message: "adapter.request.io", RequestID: "finished", BytesIn: 5, BytesOut: 7},
	})
	input := metricsRollupDistillInput{
		LogPath: path, RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName),
		Now: rollupTestTime(t, "2026-08-08T10:05:00Z"), Pricing: rollupPricingTable(), State: testRollupState(t, path),
	}
	first, err := distillMetricsRollup(t.Context(), input)
	if err != nil || first.Written != 1 {
		t.Fatalf("first pass: %+v %v", first, err)
	}
	if len(input.State.requests) != 1 || input.State.requests["ongoing"] == nil {
		t.Fatalf("first pass retained %d requests; want only ongoing", len(input.State.requests))
	}
	appendRollupLog(t, path,
		metricsHistoryRecord{Time: "2026-08-08T10:05:01Z", Message: "adapter.request.stream_opened", RequestID: "ongoing"},
		metricsHistoryRecord{Time: "2026-08-08T10:05:02Z", Message: "adapter.request.io", RequestID: "health-2", BytesOut: 16},
	)
	input.Now = input.Now.Add(metricsRollupInterval)
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil || second.Written != 0 || len(input.State.requests) != 1 || input.State.requests["ongoing"] == nil {
		t.Fatalf("streaming pass: %+v %v; retained %d requests", second, err, len(input.State.requests))
	}
	appendRollupLog(t, path,
		metricsHistoryRecord{Time: "2026-08-08T10:10:01Z", Message: "adapter.request.completed", RequestID: "ongoing", PromptTokens: 11, CompletionTokens: 13},
		metricsHistoryRecord{Time: "2026-08-08T10:10:01Z", Message: "adapter.request.io", RequestID: "ongoing", BytesIn: 17, BytesOut: 19},
	)
	input.Now = input.Now.Add(metricsRollupInterval)
	third, err := distillMetricsRollup(t.Context(), input)
	if err != nil || third.Written != 1 || len(input.State.requests) != 0 {
		t.Fatalf("terminal pass: %+v %v; retained %d requests", third, err, len(input.State.requests))
	}
	direct := BuildMetricsHistory(MetricsHistoryInput{Since: input.Now.Add(-time.Hour), Now: input.Now, LogPath: path, Pricing: input.Pricing})
	reports := metricsWindowsFromRollupPath(input.RollupPath, filepath.Join(t.TempDir(), "checkpoint"), []time.Duration{time.Hour}, input.Now, input.Pricing)
	rolled := reports[0].Report
	if metricInt(rolled.Metrics.Requests.Delta) != 2 || metricInt(rolled.Metrics.BytesOut.Delta) != 26 {
		t.Fatalf("completed lifecycles changed: requests=%d bytes_out=%d", metricInt(rolled.Metrics.Requests.Delta), metricInt(rolled.Metrics.BytesOut.Delta))
	}
	assertCounterEqual(t, "requests", direct.Metrics.Requests, rolled.Metrics.Requests)
	assertCounterEqual(t, "bytes in", direct.Metrics.BytesIn, rolled.Metrics.BytesIn)
	assertCounterEqual(t, "bytes out", direct.Metrics.BytesOut, rolled.Metrics.BytesOut)
	assertCounterEqual(t, "input tokens", direct.Metrics.InputTokens, rolled.Metrics.InputTokens)
	assertCounterEqual(t, "output tokens", direct.Metrics.OutputTokens, rolled.Metrics.OutputTokens)
}
