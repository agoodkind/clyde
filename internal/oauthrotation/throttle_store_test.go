package oauthrotation

import (
	"context"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

func TestThrottleStoreRoundTripAndExpiry(t *testing.T) {
	now := time.UnixMilli(5_000_000)
	hour := int64(time.Hour / time.Millisecond)

	tests := []struct {
		name       string
		entries    map[provider.AccountID]throttleEntry
		wantActive []provider.AccountID
	}{
		{
			name: "active entries survive round trip",
			entries: map[provider.AccountID]throttleEntry{
				"acct-a": {UntilMS: now.UnixMilli() + hour, ObservedMS: now.UnixMilli(), Claim: "five_hour", HTTPStatus: 429},
				"acct-b": {UntilMS: now.UnixMilli() + 2*hour, ObservedMS: now.UnixMilli(), Claim: "seven_day", HTTPStatus: 429},
			},
			wantActive: []provider.AccountID{"acct-a", "acct-b"},
		},
		{
			name: "expired entries dropped on load",
			entries: map[provider.AccountID]throttleEntry{
				"acct-a": {UntilMS: now.UnixMilli() - hour, ObservedMS: now.UnixMilli(), Claim: "five_hour", HTTPStatus: 429},
				"acct-b": {UntilMS: now.UnixMilli() + hour, ObservedMS: now.UnixMilli(), Claim: "seven_day", HTTPStatus: 429},
			},
			wantActive: []provider.AccountID{"acct-b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			ctx := context.Background()
			store := newThrottleStore("anthropic", nil)
			if err := store.save(ctx, now, tc.entries); err != nil {
				t.Fatalf("save: %v", err)
			}
			loaded, err := store.load(ctx, now)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(loaded) != len(tc.wantActive) {
				t.Fatalf("active count = %d, want %d (%v)", len(loaded), len(tc.wantActive), loaded)
			}
			for _, account := range tc.wantActive {
				if _, ok := loaded[account]; !ok {
					t.Fatalf("account %q missing from active set %v", account, loaded)
				}
			}
		})
	}
}

func TestThrottleStoreMissingFileIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newThrottleStore("anthropic", nil)
	loaded, err := store.load(context.Background(), time.UnixMilli(6_000_000))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("missing ledger should be empty, got %v", loaded)
	}
}

func TestThrottleStoreFlockContentionIsSafe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.UnixMilli(7_000_000)
	hour := int64(time.Hour / time.Millisecond)
	store := newThrottleStore("anthropic", nil)

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		go func(idx int) {
			defer wg.Done()
			account := provider.AccountID("acct-" + string(rune('a'+idx)))
			entry := throttleEntry{UntilMS: now.UnixMilli() + hour, ObservedMS: now.UnixMilli(), Claim: "five_hour", HTTPStatus: 429}
			errs[idx] = store.put(context.Background(), now, account, entry)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	loaded, err := store.load(context.Background(), now)
	if err != nil {
		t.Fatalf("load after contention: %v", err)
	}
	if len(loaded) != writers {
		t.Fatalf("active entries = %d, want %d; flock did not serialize writers", len(loaded), writers)
	}
}
