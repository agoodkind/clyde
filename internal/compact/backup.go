package compact

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
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
// truncates the JSONL back to PreApplyOffset (or restores from
// SnapshotPath when truncation would be unsafe).
type LedgerEntry struct {
	Timestamp      time.Time `json:"ts"`
	Op             string    `json:"op"`
	Target         int       `json:"target,omitempty"`
	Strips         []string  `json:"strips,omitempty"`
	PreApplyOffset int64     `json:"pre_apply_offset"`
	SnapshotPath   string    `json:"snapshot_path,omitempty"`
	BoundaryUUID   string    `json:"boundary_uuid,omitempty"`
	SyntheticUUID  string    `json:"synthetic_uuid,omitempty"`
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

// snapshotGzip writes a gzipped copy of the live JSONL to the
// per-session backups dir and returns the snapshot's absolute path.
// Filename is "<RFC3339-ish>-<short-uuid>.jsonl.gz".
func snapshotGzip(srcPath, sessionID string) (string, error) {
	dir, err := backupsDir(sessionID)
	if err != nil {
		return "", err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		slog.Error("compact.backup.snapshot_open_failed", "component", "compact", "path", srcPath, "err", err)
		return "", fmt.Errorf("open transcript for snapshot: %w", err)
	}
	defer func() { _ = in.Close() }()

	ts := compactClock.Now().UTC().Format("20060102-150405.000")
	short := uuid.NewString()[:8]
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl.gz", ts, short))
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		slog.Error("compact.backup.snapshot_create_failed", "component", "compact", "path", tmp, "err", err)
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		_ = gz.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_copy_failed", "component", "compact", "src", srcPath, "dst", tmp, "err", err)
		return "", fmt.Errorf("gzip copy: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_gzip_close_failed", "component", "compact", "path", tmp, "err", err)
		return "", fmt.Errorf("gzip close: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_sync_failed", "component", "compact", "path", tmp, "err", err)
		return "", fmt.Errorf("snapshot sync: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_close_failed", "component", "compact", "path", tmp, "err", err)
		return "", fmt.Errorf("snapshot close: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.backup.snapshot_rename_failed", "component", "compact", "tmp", tmp, "dst", dst, "err", err)
		return "", fmt.Errorf("snapshot rename: %w", err)
	}
	return dst, nil
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

// Undo pops the most recent ledger entry and rolls the JSONL back.
// Strategy:
//
//  1. Read every ledger entry.
//  2. Pick the last one.
//  3. Truncate the JSONL to PreApplyOffset.
//  4. Verify the file size matches; if not, restore from SnapshotPath.
//  5. Rewrite the ledger without the popped entry.
func Undo(sessionID, transcriptPath string) (LedgerEntry, error) {
	entries, err := ReadLedger(sessionID)
	if err != nil {
		return LedgerEntry{}, err
	}
	if len(entries) == 0 {
		return LedgerEntry{}, fmt.Errorf("no ledger entries to undo for session %s", sessionID)
	}
	last := entries[len(entries)-1]

	stat, err := os.Stat(transcriptPath)
	if err != nil {
		slog.Error("compact.undo.stat_failed", "component", "compact", "session_id", sessionID, "path", transcriptPath, "err", err)
		return LedgerEntry{}, fmt.Errorf("stat transcript: %w", err)
	}
	if stat.Size() < last.PreApplyOffset {
		// File is shorter than expected; truncation would be a no-op or
		// destructive in the wrong direction. Fall back to snapshot.
		if last.SnapshotPath == "" {
			return LedgerEntry{}, fmt.Errorf("transcript size %d < pre_apply_offset %d and no snapshot path", stat.Size(), last.PreApplyOffset)
		}
		if err := restoreFromSnapshot(last.SnapshotPath, transcriptPath); err != nil {
			slog.Error("compact.undo.restore_failed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "transcript", transcriptPath, "err", err)
			return LedgerEntry{}, fmt.Errorf("restore from snapshot: %w", err)
		}
	} else if err := os.Truncate(transcriptPath, last.PreApplyOffset); err != nil {
		slog.Error("compact.undo.truncate_failed", "component", "compact", "session_id", sessionID, "path", transcriptPath, "offset", last.PreApplyOffset, "err", err)
		return LedgerEntry{}, fmt.Errorf("truncate: %w", err)
	}

	// Verify post-state size matches when truncation was used.
	if final, err := os.Stat(transcriptPath); err == nil && final.Size() != last.PreApplyOffset {
		// Fall back to snapshot restore.
		if last.SnapshotPath != "" {
			if err := restoreFromSnapshot(last.SnapshotPath, transcriptPath); err != nil {
				slog.Error("compact.undo.post_truncate_restore_failed", "component", "compact", "session_id", sessionID, "snapshot", last.SnapshotPath, "transcript", transcriptPath, "err", err)
				return LedgerEntry{}, fmt.Errorf("post-truncate restore: %w", err)
			}
		}
	}

	if err := rewriteLedgerWithoutLast(sessionID); err != nil {
		slog.Error("compact.undo.rewrite_ledger_failed", "component", "compact", "session_id", sessionID, "err", err)
		return LedgerEntry{}, fmt.Errorf("rewrite ledger: %w", err)
	}
	return last, nil
}

// restoreFromSnapshot decompresses a gzipped snapshot back over the
// live transcript, atomically via a temp file plus rename.
func restoreFromSnapshot(snapshotPath, transcriptPath string) error {
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
	tmp := transcriptPath + ".restore.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		slog.Error("compact.restore.create_tmp_failed", "component", "compact", "tmp", tmp, "err", err)
		return fmt.Errorf("create restore tmp: %w", err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		slog.Error("compact.restore.decompress_failed", "component", "compact", "snapshot", snapshotPath, "tmp", tmp, "err", err)
		return fmt.Errorf("decompress: %w", err)
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
		return err
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
