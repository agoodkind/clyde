package ui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

// fakeUIClock returns a time the test can advance manually so the
// spinner-frame derivation is deterministic.
type fakeUIClock struct {
	t time.Time
}

func (c *fakeUIClock) Now() time.Time { return c.t }

func withFakeUIClock(t *testing.T, start time.Time) *fakeUIClock {
	t.Helper()
	prev := defaultUIClock
	clock := &fakeUIClock{t: start}
	defaultUIClock = clock
	t.Cleanup(func() { defaultUIClock = prev })
	return clock
}

// TestLoadingSegment_GenericStatusSpinsLive confirms that a generic
// loading status produces a Spinner=true segment whose rendered text
// changes as the clock advances. Without Spinner=true, the glyph
// would be baked in once and freeze on the first frame.
func TestLoadingSegment_GenericStatusSpinsLive(t *testing.T) {
	clock := withFakeUIClock(t, time.Unix(0, 0))

	seg := loadingSegment("probing")
	if !seg.Spinner {
		t.Fatalf("loadingSegment(probing).Spinner=false want true")
	}
	if seg.Text != "probing" {
		t.Fatalf("loadingSegment(probing).Text=%q want %q", seg.Text, "probing")
	}

	first := seg.renderedText()
	clock.t = clock.t.Add(loadingFrameInterval * 2)
	second := seg.renderedText()
	if first == second {
		t.Fatalf("spinner glyph did not advance across frames; both = %q", first)
	}
	if !strings.HasSuffix(first, " probing") || !strings.HasSuffix(second, " probing") {
		t.Fatalf("spinner suffix mismatch: %q / %q", first, second)
	}
}

// TestLoadingSegment_TerminalStatusIsStable confirms terminal statuses
// (failed:, unsupported, ...) render without a spinner so the user
// sees a steady label rather than a misleading animation.
func TestLoadingSegment_TerminalStatusIsStable(t *testing.T) {
	cases := []string{"failed: probe spawn refused", "unsupported", "cancelled", "probe_failed"}
	for _, status := range cases {
		seg := loadingSegment(status)
		if seg.Spinner {
			t.Fatalf("loadingSegment(%q).Spinner=true want false", status)
		}
		if seg.Text != status {
			t.Fatalf("loadingSegment(%q).Text=%q want %q", status, seg.Text, status)
		}
		if rendered := seg.renderedText(); rendered != status {
			t.Fatalf("rendered text=%q want unchanged %q", rendered, status)
		}
	}
}

// TestIsGenericLoadingStatus pins the canonical sentinel set the
// details pane uses to decide whether to hide the Diagnostics section.
func TestIsGenericLoadingStatus(t *testing.T) {
	for _, generic := range []string{"", "probing", "loading", "loading...", "cooldown", "refreshing", "  probing  "} {
		if !isGenericLoadingStatus(generic) {
			t.Fatalf("isGenericLoadingStatus(%q)=false want true", generic)
		}
	}
	for _, actionable := range []string{"failed: x", "unsupported", "probe_failed"} {
		if isGenericLoadingStatus(actionable) {
			t.Fatalf("isGenericLoadingStatus(%q)=true want false", actionable)
		}
	}
}

// TestApp_RepollThrottlesPerSession confirms that the per-session
// re-poll fires loadDetailAsync at most once per detailRepollInterval
// while the cached detail is still pending. Without the throttle a
// 100ms spinner tick would launch ten RPCs per second per pending
// session.
func TestApp_RepollThrottlesPerSession(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	sess := a.sessions[0]
	var probeCalls int32
	releaseLoad := make(chan struct{})
	a.cb.GetSessionDetail = func(_ context.Context, _ *session.Session) (SessionDetail, error) {
		atomic.AddInt32(&probeCalls, 1)
		<-releaseLoad
		return SessionDetail{ContextUsageStatus: "probing"}, nil
	}

	// Seed the cache with a pending detail so maybeRepollPendingDetails
	// considers this session.
	a.detailMu.Lock()
	a.detailCache[sess.Name] = SessionDetail{ContextUsageStatus: "probing"}
	a.detailMu.Unlock()

	// Drive five rapid spinner ticks. Without the throttle each one
	// would queue another loadDetailAsync.
	for range 5 {
		a.maybeRepollPendingDetails()
	}

	// First tick should have launched exactly one in-flight load and
	// set detailRepollAfter past the current clock.
	a.detailMu.Lock()
	loading := a.detailLoading[sess.Name]
	throttled := a.detailRepollAfter[sess.Name]
	a.detailMu.Unlock()
	if !loading {
		t.Fatalf("first repoll did not start an in-flight load")
	}
	if throttled.IsZero() {
		t.Fatalf("first repoll did not set detailRepollAfter")
	}

	// Allow the goroutine to finish so the test does not leak it.
	close(releaseLoad)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&probeCalls) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&probeCalls); got != 1 {
		t.Fatalf("probe calls = %d want 1 across 5 rapid ticks", got)
	}
}

// TestHandleSpinnerTick_DoesNotRebuildDetailsWhenCacheStable confirms
// that handleSpinnerTick stops rebuilding the details pane on every
// 100ms tick. Spinner segments substitute the live glyph at draw time,
// so the cached segment list animates without a rebuild. The rebuild
// is only required when the detail cache mutates, and other paths
// (handleDetailsLoaded, applySessionEvent, table navigation) cover
// that. The TextBox.contentVersion counter tracks SetSegments calls
// so we can assert the stable-cache path triggers no extra builds.
func TestHandleSpinnerTick_DoesNotRebuildDetailsWhenCacheStable(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	sess := a.sessions[0]

	// Seed a cached detail with a generic loading status so
	// detailHasPendingStatus stays true and the tick path is
	// exercised in the same conditions a real loading session sees.
	a.detailMu.Lock()
	a.detailCache[sess.Name] = SessionDetail{
		ContextUsageStatus:    "probing",
		TranscriptStatsStatus: "loading...",
	}
	a.detailMu.Unlock()
	a.selected = sess

	// Render once so the details TextBoxes settle on a known version.
	a.populateDetails()
	leftStart := a.details.Left.contentVersion
	rightStart := a.details.Right.contentVersion

	for range 10 {
		a.handleSpinnerTick()
	}

	if got := a.details.Left.contentVersion; got != leftStart {
		t.Fatalf("details.Left.contentVersion advanced %d -> %d across stable ticks; want no rebuild", leftStart, got)
	}
	if got := a.details.Right.contentVersion; got != rightStart {
		t.Fatalf("details.Right.contentVersion advanced %d -> %d across stable ticks; want no rebuild", rightStart, got)
	}
}

// TestDetailHasPendingStatus pins the predicate that drives the
// spinner ticker and the per-session re-poll loop. Cached details that
// are still settling must keep the ticker awake; loaded or terminal
// states must not.
func TestDetailHasPendingStatus(t *testing.T) {
	cases := []struct {
		name string
		d    SessionDetail
		want bool
	}{
		{
			name: "fully loaded",
			d:    SessionDetail{ContextUsageLoaded: true, TranscriptStatsLoaded: true},
			want: false,
		},
		{
			name: "context probing",
			d:    SessionDetail{ContextUsageLoaded: false, ContextUsageStatus: "probing", TranscriptStatsLoaded: true},
			want: true,
		},
		{
			name: "transcript loading",
			d:    SessionDetail{ContextUsageLoaded: true, TranscriptStatsLoaded: false, TranscriptStatsStatus: "loading..."},
			want: true,
		},
		{
			name: "context terminal failure",
			d:    SessionDetail{ContextUsageLoaded: false, ContextUsageStatus: "failed: x", TranscriptStatsLoaded: true},
			want: false,
		},
		{
			name: "transcript unsupported",
			d:    SessionDetail{ContextUsageLoaded: true, TranscriptStatsLoaded: false, TranscriptStatsStatus: "unsupported"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detailHasPendingStatus(tc.d); got != tc.want {
				t.Fatalf("detailHasPendingStatus(%+v)=%v want %v", tc.d, got, tc.want)
			}
		})
	}
}
