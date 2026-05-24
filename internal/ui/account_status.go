package ui

import (
	"time"

	"github.com/gdamore/tcell/v2"
)

// OAuthAccountInfo is the UI-facing view of one daemon-rotated OAuth account.
// It mirrors the fields the account-status model derives from without leaking
// the daemonclient or proto types past the ui boundary. The caller (cmd) maps
// its typed snapshot into this struct before handing it to the TUI.
type OAuthAccountInfo struct {
	Provider         string
	AccountID        string
	Label            string
	ExpiresAtMS      int64
	ThrottledUntilMS int64
	NeedsReauth      bool
	// Refreshing reports a transient mid-refresh state. The daemon snapshot
	// does not currently surface it, so it stays false until live state can
	// provide it; the model only emits AccountRefreshing when it is set.
	Refreshing bool
}

// AccountStatus is the derived health of one OAuth account. The model maps a
// snapshot to exactly one status using worst-severity-wins precedence:
// NeedsReauth (action required) beats Throttled (self-heals at reset) beats
// Refreshing (transient) beats Ready.
type AccountStatus int

const (
	// AccountReady means the account is usable with no operator action needed.
	AccountReady AccountStatus = iota
	// AccountRefreshing means a token refresh is in flight. Transient.
	AccountRefreshing
	// AccountThrottled means the provider has throttled the account until a
	// reset time. It self-heals once the reset passes.
	AccountThrottled
	// AccountNeedsReauth means the account can no longer refresh and requires
	// an interactive re-login.
	AccountNeedsReauth
)

// AccountStatusPresentation is the renderable form of a status: a distinct
// glyph (so states stay distinguishable without color, per Apple HIG
// accessibility), a semantic color reused from the shared palette, and a
// short human label.
type AccountStatusPresentation struct {
	Glyph rune
	Color tcell.Color
	Label string
}

// DeriveAccountStatus maps one account snapshot to its AccountStatus at time
// now. NeedsReauth wins outright. A throttle only counts while its reset is
// still in the future, so a stale throttle window self-heals to Ready without
// any daemon round-trip. Refreshing is reported only when the snapshot marks
// it and no higher-severity state applies.
func DeriveAccountStatus(account OAuthAccountInfo, now time.Time) AccountStatus {
	if account.NeedsReauth {
		return AccountNeedsReauth
	}
	if account.ThrottledUntilMS > 0 && time.UnixMilli(account.ThrottledUntilMS).After(now) {
		return AccountThrottled
	}
	if account.Refreshing {
		return AccountRefreshing
	}
	return AccountReady
}

// PresentAccountStatus returns the glyph, color, and label for a status. The
// colors are the existing semantic palette entries; this function adds no new
// color constants. The label is lower-case prose for inline use.
func PresentAccountStatus(status AccountStatus) AccountStatusPresentation {
	switch status {
	case AccountNeedsReauth:
		return AccountStatusPresentation{Glyph: '✕', Color: ColorError, Label: "needs re-auth"}
	case AccountThrottled:
		return AccountStatusPresentation{Glyph: '▲', Color: ColorWarning, Label: "throttled"}
	case AccountRefreshing:
		return AccountStatusPresentation{Glyph: '◐', Color: ColorAccent, Label: "refreshing"}
	case AccountReady:
		return AccountStatusPresentation{Glyph: '●', Color: ColorSuccess, Label: "ready"}
	default:
		return AccountStatusPresentation{Glyph: '●', Color: ColorSuccess, Label: "ready"}
	}
}
