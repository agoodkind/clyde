package compact

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyNoOpDoesNotWriteCompactBoundary(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "line one", t0),
		assistantBlocks("a1", "u1", t0.Add(time.Second), []map[string]any{
			{"type": "text", "text": "line two"},
		}),
	})

	pristine, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pristine transcript: %v", err)
	}
	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := Apply(ApplyInput{
		Slice:           slice,
		SessionID:       "session-zero-net-drop",
		Cwd:             tmp,
		Version:         "test",
		Target:          50_000,
		BoundaryTail:    []OutputBlock{{Text: "tail"}},
		PreCompactTok:   1000,
		Options:         newSynthOptions(),
		FinalProjection: 45_000,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if res == nil {
		t.Fatalf("ApplyResult = nil")
	}
	if !res.NoOp {
		t.Fatalf("NoOp = false, want true")
	}
	if res.BoundaryUUID != "" {
		t.Fatalf("BoundaryUUID = %q, want empty", res.BoundaryUUID)
	}
	if res.SyntheticUUID != "" {
		t.Fatalf("SyntheticUUID = %q, want empty", res.SyntheticUUID)
	}
	if res.SnapshotPath != "" {
		t.Fatalf("SnapshotPath = %q, want empty", res.SnapshotPath)
	}
	if res.LedgerPath != "" {
		t.Fatalf("LedgerPath = %q, want empty", res.LedgerPath)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-apply transcript: %v", err)
	}
	if !bytes.Equal(current, pristine) {
		t.Fatalf("zero-net-drop Apply mutated transcript")
	}
	if bytes.Contains(current, []byte(`"subtype":"compact_boundary"`)) {
		t.Fatalf("zero-net-drop Apply wrote compact_boundary marker")
	}
}
