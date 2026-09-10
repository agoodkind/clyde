package daemon

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDistillUnchangedInputReadsNoContent(t *testing.T) {
	input := metricsRollupDistillInput{
		LogPath: twoRequestLog(t), RollupPath: filepath.Join(t.TempDir(), metricsRollupFileName),
		Now: time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC), Pricing: rollupPricingTable(),
	}
	input.State = testRollupState(t, input.LogPath)
	first, err := distillMetricsRollup(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := distillMetricsRollup(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first read %d bytes, second read %d bytes; first wrote %d, second wrote %d", first.BytesRead, second.BytesRead, first.Written, second.Written)
	if second.BytesRead != 0 {
		t.Fatalf("unchanged pass read %d old bytes, want 0", second.BytesRead)
	}
}
