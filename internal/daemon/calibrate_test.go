package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/contextusage"
)

// stubProber is a fake contextusage.Prober used to exercise the
// CalibrateSession handler without depending on a live provider.
type stubProber struct {
	snapshot contextusage.Snapshot
	err      error
}

func (s *stubProber) Probe(_ context.Context, _ string, _ contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	return s.snapshot, s.err
}

func TestCalibrateSessionRejectsMissingSessionName(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	_, err := srv.CalibrateSession(context.Background(), &clydev1.CalibrateSessionRequest{})
	if err == nil {
		t.Fatalf("expected error for empty session_name")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("calibrate status code = %v, want InvalidArgument", got)
	}
}

func TestCalibrateSessionReturnsCalibrationFromProberSnapshot(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	createTestSession(t, "calibrate-target", "uuid-calibrate")

	contextusage.Register("claude", &stubProber{
		snapshot: contextusage.Snapshot{
			Model:       "claude-opus",
			TotalTokens: 12000,
			MaxTokens:   200000,
			Percentage:  6,
			Categories: []contextusage.Category{
				{Name: "System prompt", Tokens: 4000},
				{Name: "System tools", Tokens: 3000},
				{Name: "Messages", Tokens: 5000},
			},
		},
	})

	resp, err := srv.CalibrateSession(context.Background(), &clydev1.CalibrateSessionRequest{SessionName: "calibrate-target"})
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if resp.GetSessionName() != "calibrate-target" {
		t.Fatalf("session_name = %q want calibrate-target", resp.GetSessionName())
	}
	if resp.GetSessionId() != "uuid-calibrate" {
		t.Fatalf("session_id = %q want uuid-calibrate", resp.GetSessionId())
	}
	cal := resp.GetCalibration()
	if cal == nil {
		t.Fatalf("calibration record missing")
	}
	if cal.GetStaticOverhead() != 7000 {
		t.Fatalf("static_overhead = %d want 7000", cal.GetStaticOverhead())
	}
	if cal.GetModel() != "claude-opus" {
		t.Fatalf("model = %q want claude-opus", cal.GetModel())
	}
	if cal.GetCapturedAt() == nil || cal.GetCapturedAt().AsTime().IsZero() {
		t.Fatalf("captured_at unset")
	}
}

func TestCalibrateSessionFailsWhenProberMissing(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	createTestSession(t, "calibrate-no-prober", "uuid-no-prober")

	// Replace any previously registered claude prober with one that
	// reports an error so we can also exercise the propagation path.
	contextusage.Register("claude", &stubProber{err: errors.New("provider down")})

	_, err := srv.CalibrateSession(context.Background(), &clydev1.CalibrateSessionRequest{SessionName: "calibrate-no-prober"})
	if err == nil {
		t.Fatalf("expected error from failing prober")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("calibrate status code = %v, want Internal", got)
	}
	if !strings.Contains(err.Error(), "probe context usage") {
		t.Fatalf("error message = %q missing probe context usage wrap", err.Error())
	}
}

func TestSetCalibrationPersistsOperatorValue(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	createTestSession(t, "manual-calibrate", "uuid-manual")

	resp, err := srv.SetCalibration(context.Background(), &clydev1.SetCalibrationRequest{
		SessionName:    "manual-calibrate",
		StaticOverhead: 42_000,
	})
	if err != nil {
		t.Fatalf("set calibration: %v", err)
	}
	cal := resp.GetCalibration()
	if cal == nil {
		t.Fatalf("calibration record missing")
	}
	if cal.GetStaticOverhead() != 42_000 {
		t.Fatalf("static_overhead = %d want 42000", cal.GetStaticOverhead())
	}
	if resp.GetSessionId() != "uuid-manual" {
		t.Fatalf("session_id = %q want uuid-manual", resp.GetSessionId())
	}
	if cal.GetCapturedAt() == nil || cal.GetCapturedAt().AsTime().IsZero() {
		t.Fatalf("captured_at unset")
	}
}

func TestSetCalibrationRejectsNegativeValue(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	createTestSession(t, "negative-calibrate", "uuid-neg")

	_, err := srv.SetCalibration(context.Background(), &clydev1.SetCalibrationRequest{
		SessionName:    "negative-calibrate",
		StaticOverhead: -1,
	})
	if err == nil {
		t.Fatalf("expected error for negative static_overhead")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("set calibration status code = %v, want InvalidArgument", got)
	}
}

// TestCalibrateClientWrappersUnreachableDaemon exercises the client-
// side wrappers so deadcode considers them reachable. The wrappers
// fail to dial when no daemon socket is configured, which is the
// expected behavior in an isolated test environment.
func TestCalibrateClientWrappersUnreachableDaemon(t *testing.T) {
	_ = setupDaemonTestHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := CalibrateSessionViaDaemon(ctx, "missing"); err == nil {
		t.Fatalf("expected CalibrateSessionViaDaemon to error against unreachable daemon")
	}
	if _, err := SetCalibrationViaDaemon(ctx, "missing", 1); err == nil {
		t.Fatalf("expected SetCalibrationViaDaemon to error against unreachable daemon")
	}
}
