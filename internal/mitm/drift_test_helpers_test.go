package mitm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
)

// openTestCaptureStore opens a real capture store under a temp dir for the
// drift/baseline tests and registers its close.
func openTestCaptureStore(t *testing.T) *capture.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	return store
}

// proxyWithStore builds a minimal Proxy bound to store so the drift-capture
// writer records shapes into it. Only the store and client fields the drift
// path reads are populated.
func proxyWithStore(t *testing.T, store *capture.Store) *Proxy {
	t.Helper()
	return &Proxy{store: store, client: "test"}
}

// waitForUpstreamShapes polls the store until at least want deduped shapes are
// readable for the upstream, so a test can assert after the async drift write
// lands.
func waitForUpstreamShapes(t *testing.T, store *capture.Store, upstream string, want int) []capture.DriftShape {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		shapes, err := store.ShapesForUpstream(context.Background(), upstream, time.Unix(0, 0))
		if err == nil && len(shapes) >= want {
			return shapes
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d shapes for upstream %q (last err=%v)", want, upstream, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// upstreamShapeRowCount counts deduped drift_shapes rows for the upstream via a
// fresh read connection.
func upstreamShapeRowCount(t *testing.T, store *capture.Store, upstream string) int {
	t.Helper()
	shapes, err := store.ShapesForUpstream(context.Background(), upstream, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ShapesForUpstream: %v", err)
	}
	return len(shapes)
}
