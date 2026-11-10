package parser

import (
	"context"
	"errors"
	"testing"
)

func TestCompleteEventReadStopsAfterCancellation(t *testing.T) {
	path, _ := writeSchemaFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	count := 0
	_, err := readCompleteEvents(ctx, path, 0, 0, func(event) bool {
		count++
		cancel()
		return true
	})
	if count != 1 || !errors.Is(err, ctx.Err()) {
		t.Fatalf("canceled read visited %d events; err=%v, want one event and cancellation", count, err)
	}
}
