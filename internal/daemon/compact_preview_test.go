package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/contextusage"
)

// captureCompactStream collects CompactEvent messages emitted by the
// daemon for assertion. It implements the compactEventStream interface
// the server uses to fan events out to the gRPC server-streaming
// channel.
type captureCompactStream struct {
	events []*clydev1.CompactEvent
}

func (c *captureCompactStream) Send(ev *clydev1.CompactEvent) error {
	c.events = append(c.events, ev)
	return nil
}

// writeMetricsTranscript builds a JSONL transcript with one
// compact_boundary marker and a small post-boundary tail so the
// metrics-only path has non-zero entries, thinking blocks, image
// blocks, tool pairs, and chat turns to report.
func writeMetricsTranscript(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	defer func() { _ = f.Close() }()
	writer := bufio.NewWriter(f)
	defer func() { _ = writer.Flush() }()

	type contentBlock struct {
		Type      string `json:"type"`
		Text      string `json:"text,omitempty"`
		ToolUseID string `json:"tool_use_id,omitempty"`
		ID        string `json:"id,omitempty"`
		Name      string `json:"name,omitempty"`
	}
	type message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type entry struct {
		UUID       string    `json:"uuid"`
		ParentUUID string    `json:"parentUuid,omitempty"`
		Type       string    `json:"type"`
		Subtype    string    `json:"subtype,omitempty"`
		Timestamp  time.Time `json:"timestamp"`
		Message    *message  `json:"message,omitempty"`
	}

	now := time.Now().UTC()
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	rows := []entry{
		{
			UUID:      "boundary-uuid",
			Type:      "system",
			Subtype:   "compact_boundary",
			Timestamp: now,
		},
		{
			UUID:       "user-1",
			ParentUUID: "boundary-uuid",
			Type:       "user",
			Timestamp:  now.Add(1 * time.Second),
			Message: &message{
				Role:    "user",
				Content: []contentBlock{{Type: "text", Text: "hi"}},
			},
		},
		{
			UUID:       "assistant-1",
			ParentUUID: "user-1",
			Type:       "assistant",
			Timestamp:  now.Add(2 * time.Second),
			Message: &message{
				Role: "assistant",
				Content: []contentBlock{
					{Type: "thinking", Text: "reasoning"},
					{Type: "image"},
				},
			},
		},
	}
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			t.Fatalf("encode transcript row: %v", err)
		}
	}
}

// TestCompactPreviewMetricsOnlyStreamContract pins the daemon's
// CompactPreview contract for the metrics-only invocation that the
// dashboard at internal/cli/compact/preview.go consumes. With
// target_tokens=0 and no strippers, the stream must carry every field
// the CLI prints today, and must terminate without iteration events or
// apply mutations.
func TestCompactPreviewMetricsOnlyStreamContract(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	sess := createTestSession(t, "metrics-only", "uuid-metrics-only")
	writeMetricsTranscript(t, sess.Metadata.ProviderTranscriptPath())

	// Register a Snapshot prober so BuildRuntimeUpfront can fill in the
	// live /context fields. The fixture mirrors a calibrated session
	// with a Messages category, a Compact buffer category, and a Free
	// space category so the snapshot scalars are non-zero on the wire.
	contextusage.Register("claude", &stubProber{
		snapshot: contextusage.Snapshot{
			Model:       "claude-sonnet-4-5",
			TotalTokens: 50_000,
			MaxTokens:   200_000,
			Percentage:  25,
			Categories: []contextusage.Category{
				{Name: "System prompt", Tokens: 4_000},
				{Name: "Messages", Tokens: 30_000},
				{Name: "Compact buffer", Tokens: 13_000},
				{Name: "Free space", Tokens: 3_000},
			},
		},
	})

	// Persist a calibration record so the metrics-only path surfaces a
	// calibrated state to the stream regardless of the target=0 branch.
	cal := compactengine.Calibration{
		StaticOverhead: 7_500,
		CapturedAt:     time.Now().UTC(),
		Model:          "claude-sonnet-4-5",
	}
	if err := compactengine.SaveCalibration(sess.Metadata.ProviderSessionID(), cal); err != nil {
		t.Fatalf("save calibration: %v", err)
	}

	// Verify the calibration landed where BuildRuntimeUpfront will look.
	if _, ok, err := compactengine.LoadCalibration(sess.Metadata.ProviderSessionID()); err != nil || !ok {
		t.Fatalf("calibration not retrievable after save: ok=%v err=%v", ok, err)
	}

	stream := &captureCompactStream{}
	req := &clydev1.CompactRunRequest{
		SessionName:  "metrics-only",
		TargetTokens: 0,
		Strippers:    &clydev1.CompactStrippers{},
	}
	if err := srv.runCompact(context.Background(), req, stream, compactengine.RuntimeModePreview); err != nil {
		t.Fatalf("runCompact: %v", err)
	}

	// Contract: no iteration events, no apply mutation events.
	var upfront *clydev1.CompactUpfront
	var final *clydev1.CompactFinal
	for _, ev := range stream.events {
		switch ev.GetKind() {
		case clydev1.CompactEvent_KIND_ITERATION:
			t.Fatalf("metrics-only stream emitted iteration event: %+v", ev.GetIteration())
		case clydev1.CompactEvent_KIND_APPLY_MUTATION:
			t.Fatalf("metrics-only stream emitted apply mutation event: %+v", ev.GetApplyMutation())
		case clydev1.CompactEvent_KIND_UPFRONT:
			upfront = ev.GetUpfront()
		case clydev1.CompactEvent_KIND_FINAL:
			final = ev.GetFinal()
		case clydev1.CompactEvent_KIND_STATUS, clydev1.CompactEvent_KIND_UNSPECIFIED:
			// status events are allowed lifecycle markers
		}
	}
	if upfront == nil {
		t.Fatalf("metrics-only stream missing upfront event; got %d events", len(stream.events))
	}
	if final == nil {
		t.Fatalf("metrics-only stream missing final event")
	}

	// Snapshot scalars from the live /context probe.
	if got := upfront.GetCurrentTotal(); got != 50_000 {
		t.Fatalf("current_total = %d want 50000", got)
	}
	if got := upfront.GetMaxTokens(); got != 200_000 {
		t.Fatalf("max_tokens = %d want 200000", got)
	}
	if got := upfront.GetMessagesTokens(); got != 30_000 {
		t.Fatalf("messages_tokens = %d want 30000", got)
	}
	if got := upfront.GetCompactBufferTokens(); got != 13_000 {
		t.Fatalf("compact_buffer_tokens = %d want 13000", got)
	}
	if got := upfront.GetFreeTokens(); got != 3_000 {
		t.Fatalf("free_tokens = %d want 3000", got)
	}
	// Static overhead derives from TotalTokens minus the excluded
	// categories: 50000 - (30000 + 13000 + 3000) = 4000.
	if got := upfront.GetContextOverheadTokens(); got != 4_000 {
		t.Fatalf("context_overhead_tokens = %d want 4000", got)
	}
	if got := upfront.GetModel(); got == "" {
		t.Fatalf("model field empty on upfront event")
	}

	// Calibration state surfaced even though target=0.
	if !upfront.GetCalibrated() {
		t.Fatalf("calibrated = false; want true")
	}
	if got := upfront.GetCalibrationOverhead(); got != 7_500 {
		t.Fatalf("calibration_overhead = %d want 7500", got)
	}
	if got := upfront.GetCalibrationDate(); got == "" {
		t.Fatalf("calibration_date empty; expected YYYY-MM-DD")
	}

	// Post-boundary entry count and tail-block category counts from the
	// transcript fixture.
	if got := upfront.GetPostBoundaryEntries(); got != 2 {
		t.Fatalf("post_boundary_entries = %d want 2", got)
	}
	if got := upfront.GetThinkingBlocks(); got != 1 {
		t.Fatalf("thinking_blocks = %d want 1", got)
	}
	if got := upfront.GetImageBlocks(); got != 1 {
		t.Fatalf("image_blocks = %d want 1", got)
	}
	if got := upfront.GetChatTurns(); got != 2 {
		t.Fatalf("chat_turns = %d want 2", got)
	}
	// Tool pairs are zero in this fixture; assert the wire shape carries it.
	if got := upfront.GetToolPairs(); got != 0 {
		t.Fatalf("tool_pairs = %d want 0", got)
	}
}

// TestCompactPreviewMetricsOnlyWithoutCalibration documents the
// contract for an uncalibrated session: target=0 still emits an
// upfront event, and Calibrated is false with CalibrationOverhead
// zero.
func TestCompactPreviewMetricsOnlyWithoutCalibration(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	sess := createTestSession(t, "metrics-uncalibrated", "uuid-metrics-uncal")
	writeMetricsTranscript(t, sess.Metadata.ProviderTranscriptPath())

	contextusage.Register("claude", &stubProber{
		snapshot: contextusage.Snapshot{
			Model:       "claude-sonnet-4-5",
			TotalTokens: 10_000,
			MaxTokens:   200_000,
			Categories: []contextusage.Category{
				{Name: "Messages", Tokens: 6_000},
			},
		},
	})

	stream := &captureCompactStream{}
	req := &clydev1.CompactRunRequest{
		SessionName:  "metrics-uncalibrated",
		TargetTokens: 0,
		Strippers:    &clydev1.CompactStrippers{},
	}
	if err := srv.runCompact(context.Background(), req, stream, compactengine.RuntimeModePreview); err != nil {
		t.Fatalf("runCompact: %v", err)
	}

	var upfront *clydev1.CompactUpfront
	for _, ev := range stream.events {
		if ev.GetKind() == clydev1.CompactEvent_KIND_ITERATION {
			t.Fatalf("uncalibrated metrics-only stream emitted iteration event")
		}
		if ev.GetKind() == clydev1.CompactEvent_KIND_APPLY_MUTATION {
			t.Fatalf("uncalibrated metrics-only stream emitted apply mutation event")
		}
		if ev.GetKind() == clydev1.CompactEvent_KIND_UPFRONT {
			upfront = ev.GetUpfront()
		}
	}
	if upfront == nil {
		t.Fatalf("missing upfront event for uncalibrated session")
	}
	if upfront.GetCalibrated() {
		t.Fatalf("calibrated = true for session with no saved calibration record")
	}
	if got := upfront.GetCalibrationOverhead(); got != 0 {
		t.Fatalf("calibration_overhead = %d want 0 (no record)", got)
	}
	if got := upfront.GetCalibrationDate(); got != "" {
		t.Fatalf("calibration_date = %q want empty (no record)", got)
	}
}
