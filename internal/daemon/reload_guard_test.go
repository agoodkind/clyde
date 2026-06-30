package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestReloadDaemonWorkerRejectsAlreadyReplaced locks in that a worker whose
// reloadDrain is already set (it has handed its listeners to a replacement and
// is draining) rejects a second reload instead of asking the supervisor to
// spawn a third generation from a superseded parent. The guard returns before
// any supervisor interaction, so a nil grpcServer is never touched.
func TestReloadDaemonWorkerRejectsAlreadyReplaced(t *testing.T) {
	drained := make(chan struct{})
	close(drained)
	runtime := &runtimeServices{reloadDrain: drained}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	resp, err := reloadDaemonWorker(context.Background(), log, nil, runtime)
	if err == nil {
		t.Fatalf("reload on an already-replaced worker must error, got resp %+v", resp)
	}
	if want := "already replaced"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
	if resp != nil {
		t.Fatalf("rejected reload must return a nil response, got %+v", resp)
	}
}
