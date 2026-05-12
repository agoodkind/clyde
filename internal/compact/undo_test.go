package compact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// encodeLedgerForTest serializes ledger entries to the on-disk JSONL
// shape ReadLedger expects. Kept test-local because production code
// has no need to rewrite arbitrary entries.
func encodeLedgerForTest(t *testing.T, entries []LedgerEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal ledger entry: %v", err)
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// TestUndoRestoresFromSnapshotAfterMidFileMutation is the regression
// test for CLYDE-375. The previous Undo path called
// os.Truncate(transcriptPath, last.PreApplyOffset), which silently
// corrupted the file whenever something wrote to the live transcript
// between Apply and Undo: the size matched but the content boundary
// did not. The fix routes Undo through the recorded gzip snapshot,
// so the post-Undo bytes must be byte-identical to the pre-Apply
// bytes regardless of mid-file mutation.
func TestUndoRestoresFromSnapshotAfterMidFileMutation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		userText("u1", "", "line one", t0),
		userText("u2", "u1", "line two", t0.Add(time.Second)),
		userText("u3", "u2", "line three", t0.Add(2*time.Second)),
	}
	path := writeTranscript(t, lines)

	pristine, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pristine transcript: %v", err)
	}
	pristineHash := sha256.Sum256(pristine)

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	sessionID := "session-undo-restore"
	res, err := Apply(ApplyInput{
		Slice:         slice,
		SessionID:     sessionID,
		Cwd:           tmp,
		Version:       "test",
		Target:        100,
		BoundaryTail:  []OutputBlock{{Text: "post-apply tail"}},
		PreCompactTok: 1000,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.SnapshotPath == "" {
		t.Fatalf("Apply returned empty SnapshotPath; ledger restore is impossible")
	}

	entries, err := ReadLedger(sessionID)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].PreApplySHA256 == "" {
		t.Fatalf("ledger entry missing PreApplySHA256")
	}
	if got := entries[0].PreApplySHA256; got != hex.EncodeToString(pristineHash[:]) {
		t.Fatalf("ledger PreApplySHA256 = %s, want %s", got, hex.EncodeToString(pristineHash[:]))
	}

	// Mid-file mutation: rewrite the transcript with a single byte
	// inserted somewhere in the middle. The new size differs but the
	// key point is that the old PreApplyOffset is no longer the right
	// content boundary. The old fast path would have truncated at
	// preOffset, leaving a corrupted hybrid file.
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-apply transcript: %v", err)
	}
	mid := len(current) / 2
	mutated := make([]byte, 0, len(current)+1)
	mutated = append(mutated, current[:mid]...)
	mutated = append(mutated, 'X')
	mutated = append(mutated, current[mid:]...)
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatalf("rewrite transcript with mid-file mutation: %v", err)
	}

	if _, err := Undo(sessionID, path); err != nil {
		t.Fatalf("Undo: %v", err)
	}

	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored transcript: %v", err)
	}
	if !bytes.Equal(restored, pristine) {
		t.Fatalf("post-undo bytes differ from pristine pre-apply bytes\n got len=%d\nwant len=%d", len(restored), len(pristine))
	}
	restoredHash := sha256.Sum256(restored)
	if restoredHash != pristineHash {
		t.Fatalf("post-undo sha256 = %x, want %x", restoredHash, pristineHash)
	}
}

// TestUndoRefusesWhenSnapshotMissing verifies Undo returns a typed
// UndoSnapshotMissingError when the recorded snapshot is gone, rather
// than silently falling back to byte-offset truncation.
func TestUndoRefusesWhenSnapshotMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{userText("u1", "", "fresh", t0)})

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	sessionID := "session-undo-missing"
	res, err := Apply(ApplyInput{
		Slice:         slice,
		SessionID:     sessionID,
		Cwd:           tmp,
		Version:       "test",
		Target:        100,
		BoundaryTail:  []OutputBlock{{Text: "tail"}},
		PreCompactTok: 1000,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := os.Remove(res.SnapshotPath); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	_, err = Undo(sessionID, path)
	if err == nil {
		t.Fatalf("Undo returned nil, want UndoSnapshotMissingError")
	}
	var missing *UndoSnapshotMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want *UndoSnapshotMissingError", err)
	}
	if missing.SnapshotPath != res.SnapshotPath {
		t.Errorf("SnapshotPath = %q, want %q", missing.SnapshotPath, res.SnapshotPath)
	}
}

// TestUndoRefusesOnHashMismatch verifies Undo returns a typed
// UndoSnapshotHashMismatchError when the snapshot's decompressed
// sha256 does not match the value recorded in the ledger.
func TestUndoRefusesOnHashMismatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{userText("u1", "", "fresh", t0)})

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	sessionID := "session-undo-hash"
	if _, err := Apply(ApplyInput{
		Slice:         slice,
		SessionID:     sessionID,
		Cwd:           tmp,
		Version:       "test",
		Target:        100,
		BoundaryTail:  []OutputBlock{{Text: "tail"}},
		PreCompactTok: 1000,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Corrupt the recorded sha so the verification step fails. The
	// snapshot file itself is intact; only the ledger expectation is
	// wrong, which is exactly what a hash mismatch should detect.
	ledgerPath, err := LedgerPath(sessionID)
	if err != nil {
		t.Fatalf("LedgerPath: %v", err)
	}
	entries, err := ReadLedger(sessionID)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	entries[0].PreApplySHA256 = "deadbeef" + entries[0].PreApplySHA256[8:]
	if err := os.WriteFile(ledgerPath, encodeLedgerForTest(t, entries), 0o644); err != nil {
		t.Fatalf("rewrite ledger: %v", err)
	}

	_, err = Undo(sessionID, path)
	if err == nil {
		t.Fatalf("Undo returned nil, want UndoSnapshotHashMismatchError")
	}
	var mismatch *UndoSnapshotHashMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want *UndoSnapshotHashMismatchError", err)
	}
}
