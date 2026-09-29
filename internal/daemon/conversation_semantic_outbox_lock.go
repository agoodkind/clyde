package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// errOutboxOwned reports that another open outbox, in this or another daemon
// worker process, owns the outbox lock of the same path.
var errOutboxOwned = errors.New("another embedded ingestion worker owns the conversation semantic outbox")

// conversationSemanticOutboxLockPath returns the lock file of the outbox at
// path.
func conversationSemanticOutboxLockPath(path string) string {
	return path + ".lock"
}

// lockConversationSemanticOutbox takes an exclusive kernel lock on the outbox
// lock file without waiting. During a daemon reload the replacement worker
// starts its embedded sync before the old worker drains. The lock lets only
// one of them open the outbox. The kernel releases the lock when the returned
// file closes or the process exits. A lock that another open file owns returns
// an error that wraps errOutboxOwned.
func lockConversationSemanticOutbox(ctx context.Context, path string) (*os.File, error) {
	lockPath := conversationSemanticOutboxLockPath(path)
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
