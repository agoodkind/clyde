package daemon

import (
	"bytes"
	"testing"
	"time"

	daemonsvc "goodkind.io/clyde/internal/daemon"
)

func TestWriteMetricsHistoryReportRendersCompleteContract(t *testing.T) {
	t.Parallel()
	one := int64(1)
	two := int64(2)
	ten := int64(10)
	onePointFive := 1.5
	half := 0.5
	report := daemonsvc.MetricsHistoryReport{
		Window: daemonsvc.MetricsWindow{
			Since: time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
			Until: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		},
		Coverage: daemonsvc.MetricsCoverage{Complete: false},
		Metrics: daemonsvc.MetricsValues{
			Requests:  daemonsvc.MetricsCounter{Current: &ten, Delta: &two, RatePerSecond: &half},
			Completed: daemonsvc.MetricsCounter{}, Failed: daemonsvc.MetricsCounter{}, Cancelled: daemonsvc.MetricsCounter{},
			BytesIn: daemonsvc.MetricsCounter{}, BytesOut: daemonsvc.MetricsCounter{}, InputTokens: daemonsvc.MetricsCounter{},
			OutputTokens: daemonsvc.MetricsCounter{}, CacheTokens: daemonsvc.MetricsCounter{}, CostMicrocents: daemonsvc.MetricsCounter{},
			Inflight:  daemonsvc.MetricsGauge{Current: &one, Min: &one, Mean: &onePointFive, Max: &two},
			Streaming: daemonsvc.MetricsGauge{},
		},
		TimeBreakdown: daemonsvc.MetricsTimeBreakdown{
			Total: daemonsvc.MetricsDuration{TotalMS: &ten, Calls: &two, MeanMS: &onePointFive, P50MS: &one, P95MS: &ten, MaxMS: &ten},
			Stages: []daemonsvc.MetricsStageDuration{{
				Name:            "provider_total",
				MetricsDuration: daemonsvc.MetricsDuration{TotalMS: &ten, Calls: &two, MeanMS: &onePointFive, P50MS: &one, P95MS: &ten, MaxMS: &ten},
			}},
		},
		UnattributedDurationMS: &one,
		Warnings:               []string{"retained history starts after the requested window"},
	}
	var output bytes.Buffer
	WriteMetricsHistoryReport(&output, report)
	want := "metrics_window: since=2026-07-31T09:00:00Z until=2026-07-31T10:00:00Z\n" +
		"metrics_coverage: complete=false\n" +
		"requests: current=10 delta=2 rate_per_second=0.50\n" +
		"completed: current=n/a delta=n/a rate_per_second=n/a\n" +
		"failed: current=n/a delta=n/a rate_per_second=n/a\n" +
		"cancelled: current=n/a delta=n/a rate_per_second=n/a\n" +
		"bytes_in: current=n/a delta=n/a rate_per_second=n/a\n" +
		"bytes_out: current=n/a delta=n/a rate_per_second=n/a\n" +
		"input_tokens: current=n/a delta=n/a rate_per_second=n/a\n" +
		"output_tokens: current=n/a delta=n/a rate_per_second=n/a\n" +
		"cache_tokens: current=n/a delta=n/a rate_per_second=n/a\n" +
		"cost_microcents: current=n/a delta=n/a rate_per_second=n/a\n" +
		"inflight: current=1 min=1 mean=1.50 max=2\n" +
		"streaming: current=n/a min=n/a mean=n/a max=n/a\n" +
		"time_total: total_ms=10 calls=2 mean_ms=1.50 p50_ms=1 p95_ms=10 max_ms=10\n" +
		"time_stage: name=provider_total total_ms=10 calls=2 mean_ms=1.50 p50_ms=1 p95_ms=10 max_ms=10\n" +
		"unattributed_duration_ms: 1\n" +
		"warning: retained history starts after the requested window\n"
	if output.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", output.String(), want)
	}
}
