package daemon

import (
	"context"
	"time"

	"goodkind.io/clyde/internal/clock"
)

type ingestionClockKey struct{}

// WithIngestionClock sets the clock used to check transcript age.
//
// The daemon uses the injected clock for the growth window without changing
// the refresh cadence or other worker clocks.
//
// A caller can advance the clock after replacing a fixture:
//
//	ctx := WithIngestionClock(parent, fixtureClock.Now)
func WithIngestionClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, ingestionClockKey{}, now)
}

func ingestionClock(ctx context.Context) func() time.Time {
	if now, ok := ctx.Value(ingestionClockKey{}).(func() time.Time); ok {
		return now
	}

	return clock.Now
}
