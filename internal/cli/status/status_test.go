package status

import (
	"errors"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

func testSnapshot() statusSnapshot {
	return statusSnapshot{
		readAt: time.Unix(1770000000, 0).UTC(),
		report: daemonsvc.StatusReport{
			LaunchdTarget:          "gui/501/io.goodkind.clyde.daemon",
			LaunchdError:           "",
			SupervisorPID:          321,
			DaemonSocketPath:       "/tmp/run/clyde/daemon.sock",
			DaemonSocketExists:     true,
			DaemonResponding:       true,
			DaemonError:            "",
			SupervisorSocketPath:   "/tmp/run/clyde/daemon.supervisor.sock",
			SupervisorSocketExists: true,
			SupervisorResponding:   true,
			SupervisorError:        "",
			SupervisorFingerprint:  "development",
			WorkerPIDs:             []int{654},
			WorkerError:            "",
		},
		freshness: conversation.SearchFreshness{
			Manifest:     2900,
			Needed:       3,
			Embedded:     2897,
			Pending:      3,
			LastSyncUnix: 1770000000,
		},
		freshnessErr: nil,
		providers: daemonsvc.ProviderStatsSnapshot{
			Providers:    nil,
			LoadedAtUnix: 0,
		},
		providersErr: nil,
		mitm: daemonsvc.MITMStatus{
			Listeners: []daemonsvc.MITMListenerStatus{
				{ID: "claude-code", Address: "[::1]:48723", Up: true},
			},
			CACertPath: "",
			CAKeyPath:  "",
		},
		mitmErr: nil,
	}
}

// TestRenderLinesEmitsOneRawFactPerLine pins the output contract shared by the
// one-shot print and each terminal frame: raw name value lines a shell tool
// can cut on, with the feeder freshness and listener state present.
func TestRenderLinesEmitsOneRawFactPerLine(t *testing.T) {
	t.Parallel()

	lines := renderPlainLines(buildMetrics(testSnapshot()))
	body := strings.Join(lines, "\n")
	for _, want := range []string{
		"daemon.responding true",
		"supervisor.pid 321",
		"worker.pids 654",
		"freshness.manifest 2900 conversations",
		"freshness.needed 3 conversations",
		"freshness.pending 3 conversations",
		"mitm.claude-code.address [::1]:48723",
		"mitm.claude-code.up true",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered status lacks %q:\n%s", want, body)
		}
	}
}

// TestRenderLinesShowsSectionErrorsWithoutHidingOthers proves a dead surface
// renders as an error line while the sections that answered stay present, so a
// stopped engine never blanks the whole status.
func TestRenderLinesShowsSectionErrorsWithoutHidingOthers(t *testing.T) {
	t.Parallel()

	snapshot := testSnapshot()
	snapshot.freshnessErr = errors.New("engine unavailable")
	lines := renderPlainLines(buildMetrics(snapshot))
	body := strings.Join(lines, "\n")
	if !strings.Contains(body, `freshness.error "engine unavailable"`) {
		t.Fatalf("rendered status lacks the freshness error line:\n%s", body)
	}
	if !strings.Contains(body, "daemon.responding true") {
		t.Fatalf("a freshness error hid the daemon section:\n%s", body)
	}
}

// TestSnapshotOutputCarriesSectionErrors pins the JSON form: an errored
// section is a string under errors and its data section is absent.
func TestSnapshotOutputCarriesSectionErrors(t *testing.T) {
	t.Parallel()

	snapshot := testSnapshot()
	snapshot.providersErr = errors.New("daemon rpc: unavailable")
	out := snapshotOutput(snapshot)
	if out.Errors.Providers != "daemon rpc: unavailable" {
		t.Fatalf("Errors.Providers = %q, want the rpc error", out.Errors.Providers)
	}
	if out.Providers != nil {
		t.Fatalf("Providers = %v, want absent when the section errored", out.Providers)
	}
	if out.Freshness == nil || out.Freshness.Manifest != 2900 {
		t.Fatalf("Freshness = %+v, want the gathered snapshot", out.Freshness)
	}
}
