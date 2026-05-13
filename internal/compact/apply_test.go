package compact

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestApplyRefusesWhenProjectionOverTarget is the CLYDE-356 regression
// test. When the planner's final projection exceeds the requested
// target and ForceOverTarget is false, Apply must refuse to mutate the
// transcript and return a typed *ApplyOverTargetError. The transcript
// size and sha256 must be unchanged after the refusal.
func TestApplyRefusesWhenProjectionOverTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "line one", t0),
		userText("u2", "u1", "line two", t0.Add(time.Second)),
	})

	pristine, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pristine transcript: %v", err)
	}
	pristineHash := sha256.Sum256(pristine)
	pristineSize := int64(len(pristine))

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	const target = 200_000
	const projection = 215_190
	const wantDelta = projection - target

	_, err = Apply(ApplyInput{
		Slice:           slice,
		SessionID:       "session-over-target-refuse",
		Cwd:             tmp,
		Version:         "test",
		Target:          target,
		BoundaryTail:    []OutputBlock{{Text: "tail"}},
		PreCompactTok:   1000,
		Options:         newSynthOptions(),
		FinalProjection: projection,
		ForceOverTarget: false,
	})
	if err == nil {
		t.Fatalf("Apply returned nil, want *ApplyOverTargetError")
	}
	var overTarget *ApplyOverTargetError
	if !errors.As(err, &overTarget) {
		t.Fatalf("err = %v, want *ApplyOverTargetError", err)
	}
	if overTarget.Target != target {
		t.Errorf("Target = %d, want %d", overTarget.Target, target)
	}
	if overTarget.Projected != projection {
		t.Errorf("Projected = %d, want %d", overTarget.Projected, projection)
	}
	if overTarget.Delta != wantDelta {
		t.Errorf("Delta = %d, want %d", overTarget.Delta, wantDelta)
	}

	// The refusal message powers both the CLI exit-nonzero text and
	// the dashboard panel status row. Assert the exact format the spec
	// calls for so a copy edit of one is caught here.
	wantMessage := "apply refused: projection 215190 over target 200000 (+15190)"
	if got := overTarget.Error(); got != wantMessage {
		t.Errorf("Error() = %q, want %q", got, wantMessage)
	}

	// Transcript must be byte-identical to the pre-Apply state.
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-refusal transcript: %v", err)
	}
	if int64(len(current)) != pristineSize {
		t.Fatalf("transcript size after refusal = %d, want %d", len(current), pristineSize)
	}
	currentHash := sha256.Sum256(current)
	if currentHash != pristineHash {
		t.Fatalf("transcript sha256 after refusal = %x, want %x", currentHash, pristineHash)
	}
	if !bytes.Equal(current, pristine) {
		t.Fatalf("transcript bytes differ after refusal")
	}
}

// TestApplyProceedsWithForceOverTarget asserts that the explicit
// override flag lets Apply through and that the override emits the
// compact.apply.over_target_forced warn log so the bypass is auditable.
func TestApplyProceedsWithForceOverTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "line one", t0),
		userText("u2", "u1", "line two", t0.Add(time.Second)),
	})

	pristineSize := int64(0)
	if stat, statErr := os.Stat(path); statErr == nil {
		pristineSize = stat.Size()
	}

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	logBuf := &bytes.Buffer{}
	handler := slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	res, err := Apply(ApplyInput{
		Slice:           slice,
		SessionID:       "session-over-target-forced",
		Cwd:             tmp,
		Version:         "test",
		Target:          200_000,
		BoundaryTail:    []OutputBlock{{Text: "tail"}},
		PreCompactTok:   1000,
		Options:         newSynthOptions(),
		FinalProjection: 215_190,
		ForceOverTarget: true,
	})
	if err != nil {
		t.Fatalf("Apply with ForceOverTarget: %v", err)
	}
	if res == nil {
		t.Fatalf("ApplyResult = nil with ForceOverTarget")
	}
	if res.PostApplyOffset <= pristineSize {
		t.Fatalf("PostApplyOffset = %d, want > pristine size %d", res.PostApplyOffset, pristineSize)
	}

	postStat, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat post-apply transcript: %v", statErr)
	}
	if postStat.Size() <= pristineSize {
		t.Fatalf("transcript size after forced apply = %d, want > %d", postStat.Size(), pristineSize)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, `"msg":"compact.apply.over_target_forced"`) {
		t.Errorf("compact.apply.over_target_forced not logged; got logs:\n%s", logged)
	}
	if !strings.Contains(logged, `"target":200000`) {
		t.Errorf("over_target_forced log missing target field; got logs:\n%s", logged)
	}
	if !strings.Contains(logged, `"projection":215190`) {
		t.Errorf("over_target_forced log missing projection field; got logs:\n%s", logged)
	}
	if !strings.Contains(logged, `"delta":15190`) {
		t.Errorf("over_target_forced log missing delta field; got logs:\n%s", logged)
	}

	// Sanity: the same log line should be tagged at WARN level so it
	// shows up in audit-grade warn-only filters.
	var lines []map[string]json.RawMessage
	for raw := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
		if raw == "" {
			continue
		}
		entry := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("parse log line %q: %v", raw, err)
		}
		lines = append(lines, entry)
	}
	foundWarn := false
	for _, entry := range lines {
		var msg string
		if rawMsg, ok := entry["msg"]; ok {
			_ = json.Unmarshal(rawMsg, &msg)
		}
		if msg != "compact.apply.over_target_forced" {
			continue
		}
		var level string
		if rawLevel, ok := entry["level"]; ok {
			_ = json.Unmarshal(rawLevel, &level)
		}
		if level == "WARN" {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("compact.apply.over_target_forced not at WARN level; logs:\n%s", logged)
	}
}
