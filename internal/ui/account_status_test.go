package ui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestDeriveAccountStatus(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	future := now.Add(time.Hour).UnixMilli()
	past := now.Add(-time.Hour).UnixMilli()

	cases := []struct {
		name    string
		account OAuthAccountInfo
		want    AccountStatus
	}{
		{
			name:    "ready when nothing set",
			account: OAuthAccountInfo{},
			want:    AccountReady,
		},
		{
			name:    "needs reauth wins outright",
			account: OAuthAccountInfo{NeedsReauth: true, ThrottledUntilMS: future, Refreshing: true},
			want:    AccountNeedsReauth,
		},
		{
			name:    "throttled while reset is in the future",
			account: OAuthAccountInfo{ThrottledUntilMS: future},
			want:    AccountThrottled,
		},
		{
			name:    "stale throttle self-heals to ready",
			account: OAuthAccountInfo{ThrottledUntilMS: past},
			want:    AccountReady,
		},
		{
			name:    "throttle exactly at now is not throttled",
			account: OAuthAccountInfo{ThrottledUntilMS: now.UnixMilli()},
			want:    AccountReady,
		},
		{
			name:    "refreshing when set and no higher severity",
			account: OAuthAccountInfo{Refreshing: true},
			want:    AccountRefreshing,
		},
		{
			name:    "throttle beats refreshing",
			account: OAuthAccountInfo{ThrottledUntilMS: future, Refreshing: true},
			want:    AccountThrottled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveAccountStatus(tc.account, now)
			if got != tc.want {
				t.Fatalf("DeriveAccountStatus = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPresentAccountStatus(t *testing.T) {
	cases := []struct {
		status    AccountStatus
		wantGlyph rune
		wantColor tcell.Color
		wantLabel string
	}{
		{AccountReady, '●', ColorSuccess, "ready"},
		{AccountRefreshing, '◐', ColorAccent, "refreshing"},
		{AccountThrottled, '▲', ColorWarning, "throttled"},
		{AccountNeedsReauth, '✕', ColorError, "needs re-auth"},
	}
	for _, tc := range cases {
		got := PresentAccountStatus(tc.status)
		if got.Glyph != tc.wantGlyph {
			t.Errorf("status %d glyph = %q, want %q", tc.status, got.Glyph, tc.wantGlyph)
		}
		if got.Color != tc.wantColor {
			t.Errorf("status %d color = %v, want %v", tc.status, got.Color, tc.wantColor)
		}
		if got.Label != tc.wantLabel {
			t.Errorf("status %d label = %q, want %q", tc.status, got.Label, tc.wantLabel)
		}
	}
}

func TestPresentAccountStatusGlyphsAreDistinct(t *testing.T) {
	seen := map[rune]AccountStatus{}
	for _, status := range []AccountStatus{AccountReady, AccountRefreshing, AccountThrottled, AccountNeedsReauth} {
		glyph := PresentAccountStatus(status).Glyph
		if prior, ok := seen[glyph]; ok {
			t.Fatalf("glyph %q reused by status %d and %d; states must stay distinguishable without color", glyph, prior, status)
		}
		seen[glyph] = status
	}
}
