package compact

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

// LedgerEntry is one append in the per-session ledger.jsonl file.
// Every successful Apply writes one. --undo pops the last entry and
// restores the JSONL from SnapshotPath. PreApplyOffset and
// PreApplySHA256 are retained for diagnostic value (size and content
// checks) and are no longer used as a primary restore source.
type LedgerEntry struct {
	Timestamp      time.Time `json:"ts"`
	Op             string    `json:"op"`
	Target         int       `json:"target,omitempty"`
	Strips         []string  `json:"strips,omitempty"`
	PreApplyOffset int64     `json:"pre_apply_offset"`
	PreApplySHA256 string    `json:"pre_apply_sha256,omitempty"`
	SnapshotPath   string    `json:"snapshot_path,omitempty"`
	BoundaryUUID   string    `json:"boundary_uuid,omitempty"`
	SyntheticUUID  string    `json:"synthetic_uuid,omitempty"`
}

// UndoSnapshotMissingError is returned when the gzipped snapshot
// recorded at Apply time is not present on disk at Undo time. Undo
// refuses to proceed rather than silently truncating the live
// transcript at a stale byte offset.
type UndoSnapshotMissingError struct {
	SnapshotPath   string
	TranscriptPath string
}

// Error implements the error interface for UndoSnapshotMissingError.
func (e *UndoSnapshotMissingError) Error() string {
	if e.SnapshotPath == "" {
		return fmt.Sprintf("compact undo: ledger entry has no snapshot path; refusing to restore transcript %q", e.TranscriptPath)
	}
	return fmt.Sprintf("compact undo: snapshot %q is missing; refusing to restore transcript %q", e.SnapshotPath, e.TranscriptPath)
}

// UndoSnapshotHashMismatchError is returned when the decompressed
// snapshot's sha256 disagrees with the hash recorded in the ledger at
// Apply time. The snapshot may have been corrupted or swapped. Undo
// refuses to write a mismatched payload over the live transcript.
type UndoSnapshotHashMismatchError struct {
	SnapshotPath string
	ExpectedHash string
	ActualHash   string
}

// Error implements the error interface for UndoSnapshotHashMismatchError.
func (e *UndoSnapshotHashMismatchError) Error() string {
	return fmt.Sprintf("compact undo: snapshot %q sha256 mismatch (expected %s, got %s)", e.SnapshotPath, e.ExpectedHash, e.ActualHash)
}

// backupsDir returns the per-session backups dir under XDG state.
func backupsDir(sessionID string) (string, error) {
	root, err := SessionStateDir(sessionID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("compact.backup.mkdir_failed", "component", "compact", "session_id", sessionID, "dir", dir, "err", err)
		return "", fmt.Errorf("mkdir backups: %w", err)
	}
	return dir, nil
}

// snapshotResult bundles the on-disk location of a freshly written
// gzipped snapshot with the sha256 of the source bytes captured while
// the snapshot was written. Undo uses the hash to verify the
// decompressed snapshot matches what Apply saw.
type snapshotResult struct {
	Path      string
	SHA256Hex string
}

// snapshotGzip writes a gzipped copy of the live JSONL to the
// per-session backups dir and returns the snapshot's absolute path
// together with the sha256 of the uncompressed source bytes.
// Filename is "<RFC3339-ish>-<short-uuid>.jsonl.gz".
func snapshotGzip(srcPath, sessionID string) (snapshotResult, error) {
	dir, err := backupsDir(sessionID)
	if err != nil {
		return snapshotResult{}, err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		slog.Error("compact.backup.snapshot_open_failed", "component", "compact", "path", srcPath, "err", err)
		return snapshotResult{}, fmt.Errorf("open transcript for snapshot: %w", err)
	}
	defer func() { _ = in.Close() }()

	ts := compactClock.Now().UTC().Format("20060102-150405.000")
	short := uuid.NewString()[:8]
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl.gz", ts, short))
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		slog.Error("compact.backup.snapshot_create_failed", "component", "compact", "path", tmp, "err", err)
		return snapshotResult{}, fmt.Errorf("create snapshot: %w", err)
	}
	gz := gzip.NewWriter(out)
	hasher := sha256.New()
	// Tee the source bytes through the hasher while gzipping so we
	// capture the canonical pre-Apply hash without re-reading the file.
	mw := io.MultiWriter(gz, hasher)
	if _, err := io.Copy(mw, in); err != nil {
		_ = gz.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_copy_failed", "component", "compact", "src", srcPath, "dst", tmp, "err", err)
		return snapshotResult{}, fmt.Errorf("gzip copy: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_gzip_close_failed", "component", "compact", "path", tmp, "err", err)
		return snapshotResult{}, fmt.Errorf("gzip close: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_sync_failed", "component", "compact", "path", tmp, "err", err)
		return snapshotResult{}, fmt.Errorf("snapshot sync: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_close_failed", "component", "compact", "path", tmp, "err", err)
		return snapshotResult{}, fmt.Errorf("snapshot close: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_rename_failed", "component", "compact", "tmp", tmp, "dst", dst, "err", err)
		return snapshotResult{}, fmt.Errorf("snapshot rename: %w", err)
	}
	return snapshotResult{Path: dst, SHA256Hex: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// LedgerPath returns the absolute path of the ledger file for one
// session. Used by the CLI's --list-backups command.
func LedgerPath(sessionID string) (string, error) {
	dir, err := backupsDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ledger.jsonl"), nil
}

// appendLedger appends one entry to the per-session ledger.
func appendLedger(sessionID string, entry LedgerEntry) (string, error) {
	path, err := LedgerPath(sessionID)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		slog.Error("compact.ledger.encode_failed", "component", "compact", "session_id", sessionID, "err", err)
		return "", fmt.Errorf("encode ledger entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Error("compact.ledger.open_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return "", fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		slog.Error("compact.ledger.append_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return "", fmt.Errorf("append ledger: %w", err)
	}
	return path, nil
}

// ReadLedger returns every entry in the ledger file, oldest-first.
// Missing file returns an empty slice and no error.
func ReadLedger(sessionID string) ([]LedgerEntry, error) {
	path, err := LedgerPath(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.Error("compact.ledger.read_open_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []LedgerEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	for scanner.Scan() {
		var entry LedgerEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		slog.Error("compact.ledger.scan_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return nil, fmt.Errorf("scan ledger: %w", err)
	}
	return out, nil
}

// Undo pops the most recent ledger entry and rolls the JSONL back to
// its pre-Apply state by restoring from the gzipped snapshot recorded
// at Apply time. Strategy:
//
//  1. Read every ledger entry and pick the last one.
//  2. Refuse with UndoSnapshotMissingError if no snapshot is recorded
//     or the snapshot file is gone from disk.
//  3. Decompress the snapshot into a sibling temp file while hashing
//     the decompressed bytes. If a PreApplySHA256 is recorded in the
//     ledger and disagrees with the computed hash, refuse with
//     UndoSnapshotHashMismatchError and leave the live transcript
//     untouched.
//  4. Atomically rename the temp file over the transcript.
//  5. Rewrite the ledger without the popped entry.
//  6. Unlink the snapshot file (best-effort) now that the restore and
//     ledger pop both succeeded. Failures here are logged but do not
//     fail Undo, since the user-visible transcript is already restored.
//
// PreApplyOffset stays in the ledger entry for diagnostic value (size
// matching, audit). It is intentionally no longer used as a restore
// source because any write to the live transcript between Apply and
// Undo would leave [os.Truncate] cutting at the wrong content boundary.
func Undo(sessionID, transcriptPath string) (LedgerEntry, error) {
	entries, err := ReadLedger(sessionID)
	if err != nil {
		return LedgerEntry{}, err
	}
	if len(entries) == 0 {
		return LedgerEntry{}, fmt.Errorf("no ledger entries to undo for session %s", sessionID)
	}
	last := entries[len(entries)-1]

	if last.SnapshotPath == "" {
		refusal := &UndoSnapshotMissingError{SnapshotPath: "", TranscriptPath: transcriptPath}
		slog.Error("compact.undo.missing_snapshot_path", "component", "compact", "session_id", sessionID, "transcript", transcriptPath, "err", refusal)
		return LedgerEntry{}, refusal
	}
	if _, err := os.Stat(last.SnapshotPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			refusal := &UndoSnapshotMissingError{SnapshotPath: last.SnapshotPath, TranscriptPath: transcriptPath}
			slog.Error("compact.undo.snapshot_missing", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "transcript", transcriptPath, "err", refusal)
			return LedgerEntry{}, refusal
		}
		slog.Error("compact.undo.snapshot_stat_failed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "err", err)
		return LedgerEntry{}, fmt.Errorf("stat snapshot: %w", err)
	}

	if err := restoreFromSnapshot(last.SnapshotPath, transcriptPath, last.PreApplySHA256); err != nil {
		slog.Error("compact.undo.restore_failed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "transcript", transcriptPath, "err", err)
		return LedgerEntry{}, err
	}

	if err := rewriteLedgerWithoutLast(sessionID); err != nil {
		slog.Error("compact.undo.rewrite_ledger_failed", "component", "compact", "session_id", sessionID, "err", err)
		return LedgerEntry{}, fmt.Errorf("rewrite ledger: %w", err)
	}

	// Restore and ledger rewrite both succeeded. The snapshot has
	// fulfilled its purpose, so unlink it to prevent unbounded growth
	// of the per-session backups dir (CLYDE-374). Best-effort: failures
	// here do not roll back the user-visible restore, they just leave
	// an orphan that operator tooling can sweep later.
	var snapshotSize int64
	if info, statErr := os.Stat(last.SnapshotPath); statErr == nil {
		snapshotSize = info.Size()
	}
	if err := os.Remove(last.SnapshotPath); err != nil {
		slog.Warn("compact.undo.snapshot_unlink_failed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "err", err)
	} else {
		slog.Info("compact.undo.snapshot_removed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "size_bytes", snapshotSize)
	}

	return last, nil
}

// restoreFromSnapshot decompresses a gzipped snapshot into a sibling
// temp file in the transcript's directory, optionally verifies the
// decompressed bytes against expectedSHA256Hex, then atomically
// renames the temp file over the live transcript.
//
// expectedSHA256Hex may be empty for ledger entries written before
// the hash field existed; in that case verification is skipped and a
// debug log line records the absence.
func restoreFromSnapshot(snapshotPath, transcriptPath, expectedSHA256Hex string) error {
	in, err := os.Open(snapshotPath)
	if err != nil {
		slog.Error("compact.restore.open_snapshot_failed", "component", "compact", "snapshot", snapshotPath, "err", err)
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = in.Close() }()
	gz, err := gzip.NewReader(in)
	if err != nil {
		slog.Error("compact.restore.gzip_open_failed", "component", "compact", "snapshot", snapshotPath, "err", err)
		return fmt.Errorf("gzip open: %w", err)
	}
	defer func() { _ = gz.Close() }()
	// Place the temp file in the same directory as the transcript so
	// the final rename is on the same filesystem and stays atomic.
	tmp := transcriptPath + ".restore.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		slog.Error("compact.restore.create_tmp_failed", "component", "compact", "tmp", tmp, "err", err)
		return fmt.Errorf("create restore tmp: %w", err)
	}
	hasher := sha256.New()
	var sink io.Writer = out
	if expectedSHA256Hex != "" {
		sink = io.MultiWriter(out, hasher)
	}
	if _, err := io.Copy(sink, gz); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.restore.decompress_failed", "component", "compact", "snapshot", snapshotPath, "tmp", tmp, "err", err)
		return fmt.Errorf("decompress: %w", err)
	}
	if expectedSHA256Hex != "" {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != expectedSHA256Hex {
			_ = out.Close()
			_ = os.Remove(tmp)
			mismatch := &UndoSnapshotHashMismatchError{SnapshotPath: snapshotPath, ExpectedHash: expectedSHA256Hex, ActualHash: actual}
			slog.Error("compact.restore.hash_mismatch", "component", "compact", "snapshot", snapshotPath, "expected", expectedSHA256Hex, "actual", actual, "err", mismatch)
			return mismatch
		}
	} else {
		slog.Debug("compact.restore.hash_skipped", "component", "compact", "snapshot", snapshotPath, "reason", "ledger entry predates pre_apply_sha256 field")
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.restore.sync_failed", "component", "compact", "tmp", tmp, "err", err)
		return fmt.Errorf("sync restore: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.restore.close_failed", "component", "compact", "tmp", tmp, "err", err)
		return fmt.Errorf("close restore: %w", err)
	}
	if err := os.Rename(tmp, transcriptPath); err != nil {
		slog.Error("compact.restore.rename_failed", "component", "compact", "tmp", tmp, "transcript", transcriptPath, "err", err)
		return fmt.Errorf("rename restore: %w", err)
	}
	return nil
}

// rewriteLedgerWithoutLast atomically rewrites the ledger file with
// every entry except the last one.
func rewriteLedgerWithoutLast(sessionID string) error {
	path, err := LedgerPath(sessionID)
	if err != nil {
		return err
	}
	entries, err := ReadLedger(sessionID)
	if err != nil {
		return err
	}
	if len(entries) <= 1 {
		return os.Remove(path)
	}
	keep := entries[:len(entries)-1]
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		slog.Error("compact.ledger.rewrite_create_failed", "component", "compact", "session_id", sessionID, "tmp", tmp, "err", err)
		return fmt.Errorf("create tmp ledger: %w", err)
	}
	for _, entry := range keep {
		encoded, err := json.Marshal(entry)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
		if _, err := f.Write(append(encoded, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// SortLedger returns entries sorted by Timestamp descending.
func SortLedger(entries []LedgerEntry) []LedgerEntry {
	out := append([]LedgerEntry(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}
