package hookspec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// ReorientSnapshotKey identifies a pre-compact snapshot across paired hook
// events without depending on provider-specific ids.
type ReorientSnapshotKey struct {
	TranscriptPath string `json:"transcript_path"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
}

// FileSnapshotStore stores hook snapshots in Clyde's user state directory.
type FileSnapshotStore struct {
	Root string
}

// NewFileSnapshotStore creates a snapshot store under the Clyde state root when
// root is empty.
func NewFileSnapshotStore(root string) FileSnapshotStore {
	if strings.TrimSpace(root) != "" {
		return FileSnapshotStore{Root: root}
	}
	return FileSnapshotStore{Root: filepath.Join(config.DefaultStateDir(), "hooks", "reorient")}
}

// Save writes a snapshot atomically.
func (store FileSnapshotStore) Save(ctx context.Context, key ReorientSnapshotKey, body string) error {
	if err := ctx.Err(); err != nil {
		wrapped := fmt.Errorf("save reorient snapshot canceled: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "err", wrapped)
		return wrapped
	}
	path := store.pathForKey(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		wrapped := fmt.Errorf("create reorient snapshot dir: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*.tmp")
	if err != nil {
		wrapped := fmt.Errorf("create reorient snapshot temp: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if _, err := temp.WriteString(body); err != nil {
		wrapped := fmt.Errorf("write reorient snapshot temp: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	if err := temp.Chmod(0o600); err != nil {
		wrapped := fmt.Errorf("chmod reorient snapshot temp: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	if err := temp.Close(); err != nil {
		closed = true
		wrapped := fmt.Errorf("close reorient snapshot temp: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	closed = true
	if err := os.Rename(tempPath, path); err != nil {
		wrapped := fmt.Errorf("install reorient snapshot: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.save_failed", "component", "hooks", "path", path, "err", wrapped)
		return wrapped
	}
	slog.InfoContext(ctx, "hooks.reorient_snapshot.saved", "component", "hooks", "path", path, "bytes", len(body))
	return nil
}

// Consume reads and removes a snapshot. A missing snapshot returns ok=false.
func (store FileSnapshotStore) Consume(ctx context.Context, key ReorientSnapshotKey) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		wrapped := fmt.Errorf("consume reorient snapshot canceled: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.consume_failed", "component", "hooks", "err", wrapped)
		return "", false, wrapped
	}
	path := store.pathForKey(key)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		wrapped := fmt.Errorf("read reorient snapshot: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.consume_failed", "component", "hooks", "path", path, "err", wrapped)
		return "", false, wrapped
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		wrapped := fmt.Errorf("remove reorient snapshot: %w", err)
		slog.WarnContext(ctx, "hooks.reorient_snapshot.consume_failed", "component", "hooks", "path", path, "err", wrapped)
		return "", false, wrapped
	}
	slog.InfoContext(ctx, "hooks.reorient_snapshot.consumed", "component", "hooks", "path", path, "bytes", len(body))
	return string(body), true, nil
}

func (store FileSnapshotStore) pathForKey(key ReorientSnapshotKey) string {
	normalized := normalizeSnapshotKey(key)
	body, err := json.Marshal(normalized)
	if err != nil {
		slog.Warn("hooks.reorient_snapshot.key_marshal_failed", "component", "hooks", "err", err)
		body = []byte(normalized.TranscriptPath + "\x00" + normalized.ConversationID + "\x00" + normalized.SessionID + "\x00" + normalized.CWD)
	}
	sum := sha256.Sum256(body)
	root := strings.TrimSpace(store.Root)
	if root == "" {
		root = filepath.Join(config.DefaultStateDir(), "hooks", "reorient")
	}
	return filepath.Join(root, hex.EncodeToString(sum[:])+".txt")
}

func normalizeSnapshotKey(key ReorientSnapshotKey) ReorientSnapshotKey {
	normalized := ReorientSnapshotKey{
		TranscriptPath: strings.TrimSpace(key.TranscriptPath),
		ConversationID: strings.TrimSpace(key.ConversationID),
		SessionID:      strings.TrimSpace(key.SessionID),
		CWD:            strings.TrimSpace(key.CWD),
	}
	if normalized.TranscriptPath != "" {
		normalized.ConversationID = normalized.TranscriptPath
		normalized.TranscriptPath = ""
	}
	return normalized
}
