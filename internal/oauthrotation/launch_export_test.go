package oauthrotation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// TestSelectForLaunchPicksReadyAccount confirms SelectForLaunch returns the
// first non-throttled account and its stored .credentials.json bytes, mirroring
// Token's selection.
func TestSelectForLaunchPicksReadyAccount(t *testing.T) {
	now := time.UnixMilli(5_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	account, encoded, err := rot.SelectForLaunch(ctx, prov.Name())
	if err != nil {
		t.Fatalf("SelectForLaunch: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a", account)
	}

	var stored fakeStored
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatalf("decode planted credentials: %v", err)
	}
	if stored.Account != "acct-a" {
		t.Fatalf("planted account = %q, want acct-a", stored.Account)
	}
	if stored.AccessToken != "acct-a:token" {
		t.Fatalf("planted access token = %q, want acct-a:token", stored.AccessToken)
	}
}

// TestSelectForLaunchHonorsThrottle confirms a throttle on the first account
// moves the launch selection to the next non-throttled account, identical to
// Token.
func TestSelectForLaunchHonorsThrottle(t *testing.T) {
	now := time.UnixMilli(6_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	store := newThrottleStore(prov.Name(), nil)
	entry := throttleEntry{
		UntilMS:    now.Add(time.Hour).UnixMilli(),
		ObservedMS: now.UnixMilli(),
		Claim:      string(ratelimitsink.ClaimFiveHour),
		HTTPStatus: 429,
	}
	if err := store.put(ctx, now, provider.AccountID("acct-a"), entry); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}

	account, encoded, err := rot.SelectForLaunch(ctx, prov.Name())
	if err != nil {
		t.Fatalf("SelectForLaunch: %v", err)
	}
	if account != "acct-b" {
		t.Fatalf("account = %q, want acct-b after acct-a throttle", account)
	}
	var stored fakeStored
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatalf("decode planted credentials: %v", err)
	}
	if stored.Account != "acct-b" {
		t.Fatalf("planted account = %q, want acct-b", stored.Account)
	}
}

// TestSelectForLaunchAllThrottled confirms SelectForLaunch surfaces the typed
// AllAccountsThrottledError (and no bytes) when every account is throttled.
func TestSelectForLaunchAllThrottled(t *testing.T) {
	now := time.UnixMilli(7_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	store := newThrottleStore(prov.Name(), nil)
	for _, acct := range []string{"acct-a", "acct-b"} {
		entry := throttleEntry{
			UntilMS:    now.Add(time.Hour).UnixMilli(),
			ObservedMS: now.UnixMilli(),
			Claim:      string(ratelimitsink.ClaimFiveHour),
			HTTPStatus: 429,
		}
		if err := store.put(ctx, now, provider.AccountID(acct), entry); err != nil {
			t.Fatalf("seed throttle %q: %v", acct, err)
		}
	}

	account, encoded, err := rot.SelectForLaunch(ctx, prov.Name())
	var typed AllAccountsThrottledError
	if !errors.As(err, &typed) {
		t.Fatalf("want AllAccountsThrottledError, got %v", err)
	}
	if account != "" || encoded != nil {
		t.Fatalf("want empty result on all-throttled, got account=%q encoded=%v", account, encoded)
	}
}

// TestSelectForLaunchUnknownProvider confirms an unregistered provider yields an
// error and no bytes.
func TestSelectForLaunchUnknownProvider(t *testing.T) {
	now := time.UnixMilli(8_000_000)
	ctx := context.Background()
	rot, _ := seedRotator(t, now, "acct-a")

	account, encoded, err := rot.SelectForLaunch(ctx, provider.Name("nonesuch"))
	if err == nil {
		t.Fatalf("SelectForLaunch for unknown provider: want error, got nil")
	}
	if account != "" || encoded != nil {
		t.Fatalf("want empty result for unknown provider, got account=%q encoded=%v", account, encoded)
	}
}
