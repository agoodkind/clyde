package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// errOutboxOwned reports a conflicting recovery or publication operation.
var errOutboxOwned = errors.New("another embedded ingestion worker owns the conversation semantic outbox")

// conversationSemanticOutboxLockPath returns the lock file of the outbox at
// path.
func conversationSemanticOutboxLockPath(path string) string {
	return path + ".lock"
}

// lockConversationSemanticOutbox serializes recovery and publication across
// daemon generations without waiting. The caller closes the returned file
// after its operation. Lock contention returns an error wrapping errOutboxOwned.
func lockConversationSemanticOutbox(ctx context.Context, path string) (*os.File, error) {
	lockPath := conversationSemanticOutboxLockPath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("create conversation semantic outbox directory %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.lock_open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open conversation semantic outbox lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errOutboxOwned
		}
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.lock_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, errors.Join(fmt.Errorf("lock conversation semantic outbox %s: %w", lockPath, err), closeErr)
	}
	return file, nil
}
