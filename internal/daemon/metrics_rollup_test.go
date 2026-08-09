package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rollupTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func writeRollupFixture(t *testing.T, records []metricsRollupRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), metricsRollupFileName)
	if err := appendMetricsRollupRecords(path, records); err != nil {
		t.Fatalf("append rollup records: %v", err)
	}
	return path
}

func requestRollupFixture(id string, started string, streamed string, terminal string) metricsRollupRecord {
	return metricsRollupRecord{
		Kind:                metricsRollupKindRequest,
		RequestID:           id,
		StartedAt:           started,
		StreamedAt:          streamed,
		TerminalAt:          terminal,
		Status:              metricsRequestStatusCompleted,
		Model:               "claude-opus-4-8",
		BytesIn:             10,
		BytesOut:            20,
		PromptTokens:        100,
		OutputTokens:        50,
		CacheReadTokens:     5,
		CacheCreationTokens: 3,
		GenerationStartedAt: "",
	}
}

// TestLoadMetricsRollupBucketsOneReadIntoEveryWindow is the core claim of the
// rollup: one scan of the store answers several windows at once, and a record
// lands in exactly the windows whose interval contains its terminal instant.
func TestLoadMetricsRollupBucketsOneReadIntoEveryWindow(t *testing.T) {
	now := rollupTestTime(t, "2026-08-08T12:00:00Z")
	path := writeRollupFixture(t, []metricsRollupRecord{
		requestRollupFixture("recent", "2026-08-08T11:59:00Z", "2026-08-08T11:59:01Z", "2026-08-08T11:59:02Z"),
		requestRollupFixture("today", "2026-08-08T03:00:00Z", "2026-08-08T03:00:01Z", "2026-08-08T03:00:02Z"),
		requestRollupFixture("thisweek", "2026-08-04T03:00:00Z", "2026-08-04T03:00:01Z", "2026-08-04T03:00:02Z"),
		requestRollupFixture("tooold", "2026-07-01T03:00:00Z", "2026-07-01T03:00:01Z", "2026-07-01T03:00:02Z"),
	})

	windows := []MetricsWindow{
		{Since: now.Add(-time.Hour), Until: now},
		{Since: now.Add(-24 * time.Hour), Until: now},
		{Since: now.Add(-7 * 24 * time.Hour), Until: now},
	}
	loaded, err := loadMetricsRollup(path, windows)
	if err != nil {
		t.Fatalf("load rollup: %v", err)
	}

	expectedCounts := []int{1, 2, 3}
	for i, expected := range expectedCounts {
		if len(loaded[i].Requests) != expected {
			t.Fatalf("window %d has %d requests, want %d", i, len(loaded[i].Requests), expected)
		}
	}
	if _, ok := loaded[0].Requests["recent"]; !ok {
		t.Fatal("hour window missing the request that finished inside it")
	}
	if _, ok := loaded[2].Requests["tooold"]; ok {
		t.Fatal("week window included a request older than the window")
	}
}

// TestLoadMetricsRollupCountsRestartsPerWindow proves the restart count is
// scoped to each window, which is what makes a summed wide window trustworthy:
// the reader can see how many counter discontinuities it spans.
func TestLoadMetricsRollupCountsRestartsPerWindow(t *testing.T) {
	now := rollupTestTime(t, "2026-08-08T12:00:00Z")
	// One generation lands in each successively wider window: 11:30 today is
	// inside the hour, 20:00 yesterday is inside the day but not the hour, and
	// the two older ones are inside the week only.
	path := writeRollupFixture(t, []metricsRollupRecord{
		generationRollupRecord(rollupTestTime(t, "2026-07-20T09:00:00Z")),
		generationRollupRecord(rollupTestTime(t, "2026-08-02T09:00:00Z")),
		generationRollupRecord(rollupTestTime(t, "2026-08-07T09:00:00Z")),
		generationRollupRecord(rollupTestTime(t, "2026-08-07T20:00:00Z")),
		generationRollupRecord(rollupTestTime(t, "2026-08-08T11:30:00Z")),
	})

	windows := []MetricsWindow{
		{Since: now.Add(-time.Hour), Until: now},
		{Since: now.Add(-24 * time.Hour), Until: now},
		{Since: now.Add(-7 * 24 * time.Hour), Until: now},
	}
	loaded, err := loadMetricsRollup(path, windows)
	if err != nil {
		t.Fatalf("load rollup: %v", err)
	}

	expectedRestarts := []int{1, 2, 4}
	for i, expected := range expectedRestarts {
		if loaded[i].Restarts != expected {
			t.Fatalf("window %d counted %d restarts, want %d", i, loaded[i].Restarts, expected)
		}
	}
}

// TestRollupRoundTripPreservesAggregationInputs checks that a request survives
// the store unchanged in every field the aggregation reads. A dropped field
// here would silently zero a counter in the reported window.
func TestRollupRoundTripPreservesAggregationInputs(t *testing.T) {
	original := &metricsRequest{
		started: true, terminal: true, lifecycleStarted: true, lifecycleTerminal: true,
		invalidLifecycle: false, status: metricsRequestStatusFailed, totalMS: 0,
		bytesIn: 4096, bytesOut: 8192, promptTokens: 1200, outputTokens: 340,
		cacheTokens: 0, cacheReadTokens: 700, cacheCreationTokens: 250,
		model: "gpt-5", ioSeen: true,
		startedAt:    rollupTestTime(t, "2026-08-08T10:00:00Z"),
		streamedAt:   rollupTestTime(t, "2026-08-08T10:00:02Z"),
		terminalAt:   rollupTestTime(t, "2026-08-08T10:00:09Z"),
		legDurations: nil,
	}

	path := writeRollupFixture(t, []metricsRollupRecord{requestToRollupRecord("req-1", original)})
	loaded, err := loadMetricsRollup(path, []MetricsWindow{{
		Since: rollupTestTime(t, "2026-08-08T09:00:00Z"),
		Until: rollupTestTime(t, "2026-08-08T11:00:00Z"),
	}})
	if err != nil {
		t.Fatalf("load rollup: %v", err)
	}
	restored, ok := loaded[0].Requests["req-1"]
	if !ok {
		t.Fatal("round-tripped request missing from window")
	}

	if restored.status != original.status {
		t.Fatalf("status=%q want %q", restored.status, original.status)
	}
	if restored.model != original.model {
		t.Fatalf("model=%q want %q", restored.model, original.model)
	}
	if restored.bytesIn != original.bytesIn || restored.bytesOut != original.bytesOut {
		t.Fatalf("bytes in=%d out=%d want in=%d out=%d",
			restored.bytesIn, restored.bytesOut, original.bytesIn, original.bytesOut)
	}
	if restored.promptTokens != original.promptTokens || restored.outputTokens != original.outputTokens {
		t.Fatalf("tokens prompt=%d output=%d want prompt=%d output=%d",
			restored.promptTokens, restored.outputTokens, original.promptTokens, original.outputTokens)
	}
	if restored.cacheReadTokens != original.cacheReadTokens ||
		restored.cacheCreationTokens != original.cacheCreationTokens {
		t.Fatalf("cache read=%d creation=%d want read=%d creation=%d",
			restored.cacheReadTokens, restored.cacheCreationTokens,
			original.cacheReadTokens, original.cacheCreationTokens)
	}
	if !restored.startedAt.Equal(original.startedAt) ||
		!restored.streamedAt.Equal(original.streamedAt) ||
		!restored.terminalAt.Equal(original.terminalAt) {
		t.Fatalf("lifecycle instants drifted: started=%s streamed=%s terminal=%s",
			restored.startedAt, restored.streamedAt, restored.terminalAt)
	}
}

// TestRollupOmitsStreamedWhenRequestNeverStreamed pins the absence contract.
// The aggregation picks the provider_total stage exactly when a request has no
// stream instant, so a zero-valued streamed time would silently reclassify the
// request into the two streaming stages.
func TestRollupOmitsStreamedWhenRequestNeverStreamed(t *testing.T) {
	nonStreaming := &metricsRequest{
		started: true, terminal: true, lifecycleStarted: true, lifecycleTerminal: true,
		invalidLifecycle: false, status: metricsRequestStatusCompleted, totalMS: 0,
		bytesIn: 0, bytesOut: 0, promptTokens: 0, outputTokens: 0, cacheTokens: 0,
		cacheReadTokens: 0, cacheCreationTokens: 0, model: "", ioSeen: false,
		startedAt:    rollupTestTime(t, "2026-08-08T10:00:00Z"),
		streamedAt:   time.Time{},
		terminalAt:   rollupTestTime(t, "2026-08-08T10:00:05Z"),
		legDurations: nil,
	}
	record := requestToRollupRecord("req-plain", nonStreaming)
	if record.StreamedAt != "" {
		t.Fatalf("streamed_at=%q, want empty for a request that never streamed", record.StreamedAt)
	}

	path := writeRollupFixture(t, []metricsRollupRecord{record})
	loaded, err := loadMetricsRollup(path, []MetricsWindow{{
		Since: rollupTestTime(t, "2026-08-08T09:00:00Z"),
		Until: rollupTestTime(t, "2026-08-08T11:00:00Z"),
	}})
	if err != nil {
		t.Fatalf("load rollup: %v", err)
	}
	restored := loaded[0].Requests["req-plain"]
	if !restored.streamedAt.IsZero() {
		t.Fatalf("streamed instant=%s, want zero", restored.streamedAt)
	}
	stages := exclusiveStageSamples(restored)
	if _, ok := stages["provider_total"]; !ok {
		t.Fatalf("stages=%v, want provider_total for a non-streaming request", stages)
	}
}

// TestPruneMetricsRollupDropsOnlyExpiredRecords verifies retention trims the
// store from the old end and leaves everything inside retention intact.
func TestPruneMetricsRollupDropsOnlyExpiredRecords(t *testing.T) {
	path := writeRollupFixture(t, []metricsRollupRecord{
		requestRollupFixture("old", "2026-07-01T03:00:00Z", "", "2026-07-01T03:00:02Z"),
		generationRollupRecord(rollupTestTime(t, "2026-07-01T04:00:00Z")),
		requestRollupFixture("kept", "2026-08-08T03:00:00Z", "", "2026-08-08T03:00:02Z"),
	})

	cutoff := rollupTestTime(t, "2026-08-01T00:00:00Z")
	dropped, err := pruneMetricsRollup(path, cutoff)
	if err != nil {
		t.Fatalf("prune rollup: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("dropped=%d, want 2", dropped)
	}

	loaded, err := loadMetricsRollup(path, []MetricsWindow{{
		Since: rollupTestTime(t, "2026-06-01T00:00:00Z"),
		Until: rollupTestTime(t, "2026-09-01T00:00:00Z"),
	}})
	if err != nil {
		t.Fatalf("load rollup: %v", err)
	}
	if len(loaded[0].Requests) != 1 {
		t.Fatalf("kept %d requests, want 1", len(loaded[0].Requests))
	}
	if _, ok := loaded[0].Requests["kept"]; !ok {
		t.Fatal("prune dropped a record inside retention")
	}
	if loaded[0].Restarts != 0 {
		t.Fatalf("restarts=%d, want 0 after the old generation record aged out", loaded[0].Restarts)
	}
}

// TestPruneMetricsRollupKeepsUnparsableLines states the safety bias: a line the
// reader cannot date is retained, because dropping it would shrink the window
// without anything reporting the loss.
func TestPruneMetricsRollupKeepsUnparsableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), metricsRollupFileName)
	if err := os.WriteFile(path, []byte("{not json\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dropped, err := pruneMetricsRollup(path, rollupTestTime(t, "2026-09-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("prune rollup: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("dropped=%d, want 0", dropped)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("prune emptied a file it could not date")
	}
}

// TestLoadMetricsRollupMissingStoreIsEmptyNotAnError covers first run, before
// the daemon has written anything: the status command must report an empty
// window rather than fail.
func TestLoadMetricsRollupMissingStoreIsEmptyNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), metricsRollupFileName)
	loaded, err := loadMetricsRollup(path, []MetricsWindow{{
		Since: rollupTestTime(t, "2026-08-08T09:00:00Z"),
		Until: rollupTestTime(t, "2026-08-08T11:00:00Z"),
	}})
	if err != nil {
		t.Fatalf("missing store returned an error: %v", err)
	}
	if len(loaded) != 1 || len(loaded[0].Requests) != 0 || loaded[0].Found {
		t.Fatalf("missing store did not read as empty: %+v", loaded)
	}
}

// TestMetricsRollupCheckpointRoundTrip covers the writer's resume state, which
// is what keeps each pass reading only the new tail of the daemon log.
func TestMetricsRollupCheckpointRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	if empty := readMetricsRollupCheckpoint(path); empty.LastRecordAt != "" {
		t.Fatalf("missing checkpoint did not read as zero: %+v", empty)
	}
	want := metricsRollupCheckpoint{LastRecordAt: "2026-08-08T10:00:00Z"}
	if err := writeMetricsRollupCheckpoint(path, want); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	got := readMetricsRollupCheckpoint(path)
	if got != want {
		t.Fatalf("checkpoint round trip: got %+v want %+v", got, want)
	}
}

// TestSortedRollupRecordsOrdersByInstant keeps the store append-ordered when a
// pass emits requests from a map, whose iteration order is random.
func TestSortedRollupRecordsOrdersByInstant(t *testing.T) {
	sorted := sortedRollupRecords([]metricsRollupRecord{
		requestRollupFixture("third", "", "", "2026-08-08T03:00:00Z"),
		generationRollupRecord(rollupTestTime(t, "2026-08-08T01:00:00Z")),
		requestRollupFixture("second", "", "", "2026-08-08T02:00:00Z"),
	})
	wantOrder := []string{"", "second", "third"}
	for i, want := range wantOrder {
		if sorted[i].RequestID != want {
			t.Fatalf("position %d has request_id %q, want %q", i, sorted[i].RequestID, want)
		}
	}
}
