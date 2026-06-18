package mitm

import (
	"encoding/json"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/mitm/capture"
)

// driftShapeForChurnTest builds a claude /v1/messages drift shape through the
// proxy's capture path so the fingerprint and templated path reflect production
// behavior.
func driftShapeForChurnTest(path, contentLen, sessionID, userAgent string) capture.DriftShape {
	header := http.Header{}
	header.Set("User-Agent", userAgent)
	header.Set("Anthropic-Version", "2023-06-01")
	header.Set("Content-Length", contentLen)
	header.Set("X-Claude-Code-Session-Id", sessionID)
	return buildDriftShape(driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        path,
		upstreamURL: "https://api.anthropic.com" + path,
		header:      header,
		body:        []byte(`{"model":"claude-x","messages":[]}`),
	}, driftUpstreamClaudeCode)
}

// TestDriftFingerprintCollapsesVolatilePathAndHeaders proves the corpus dedupes:
// two requests that differ only by a per-session path id, a per-session header
// id, and request byte size share one fingerprint, and the stored path is
// templated.
func TestDriftFingerprintCollapsesVolatilePathAndHeaders(t *testing.T) {
	t.Parallel()
	a := driftShapeForChurnTest(
		"/v1/code/sessions/cse_01GCDZ4cWW1qo9SHkFuaPZhf/worker/events",
		"100", "11111111-1111-1111-1111-111111111111", "claude-cli/2.1.123 (external, sdk-cli)")
	b := driftShapeForChurnTest(
		"/v1/code/sessions/cse_01L6yMMZjNBP5nDaPqbJdeSx/worker/events",
		"999999", "22222222-2222-2222-2222-222222222222", "claude-cli/2.1.123 (external, sdk-cli)")
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("fingerprints differ across sessions for the same shape:\n a=%s\n b=%s", a.Fingerprint, b.Fingerprint)
	}
	if a.Path != "/v1/code/sessions/{id}/worker/events" {
		t.Fatalf("stored path not templated: %q", a.Path)
	}
}

// TestDriftFingerprintDistinguishesRealShapes proves the dedup is not too
// aggressive: a genuine identity difference (the user-agent) still produces a
// distinct fingerprint.
func TestDriftFingerprintDistinguishesRealShapes(t *testing.T) {
	t.Parallel()
	a := driftShapeForChurnTest("/v1/messages", "100", "11111111-1111-1111-1111-111111111111", "claude-cli/2.1.123 (external, sdk-cli)")
	b := driftShapeForChurnTest("/v1/messages", "100", "11111111-1111-1111-1111-111111111111", "claude-cli/9.9.999 (external, sdk-cli)")
	if a.Fingerprint == b.Fingerprint {
		t.Fatalf("fingerprints collapsed for distinct user-agents: %s", a.Fingerprint)
	}
}

// snapshotFromHeaders builds a single-shape snapshot for the diff tests.
func snapshotFromHeaders(t *testing.T, headers map[string]string) Snapshot {
	t.Helper()
	shape := capture.DriftShape{
		Upstream:       "claude-code",
		Method:         http.MethodPost,
		Path:           "/v1/messages",
		RequestHeaders: headers,
		RequestBody:    json.RawMessage(`{"mode":"filtered_inline","body_type":"json_object","keys":["messages","model"]}`),
		SeenCount:      1,
	}
	snap, err := ExtractSnapshotFromShapes([]capture.DriftShape{shape}, SnapshotOptions{})
	if err != nil {
		t.Fatalf("ExtractSnapshotFromShapes: %v", err)
	}
	return snap
}

func findFlavorHeader(t *testing.T, snap Snapshot, name string) Header {
	t.Helper()
	if len(snap.Flavors) != 1 {
		t.Fatalf("flavor count = %d, want 1", len(snap.Flavors))
	}
	for _, h := range snap.Flavors[0].Headers {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("header %q not found in flavor", name)
	return Header{}
}

// TestVolatileHeaderDifferenceDoesNotDiverge proves the difference matrix is not
// polluted: a baseline differing only in content-length and session-id does not
// read as drift, and those headers are marked volatile with no stored values.
func TestVolatileHeaderDifferenceDoesNotDiverge(t *testing.T) {
	t.Parallel()
	ref := snapshotFromHeaders(t, map[string]string{
		"user-agent":               "claude-cli/2.1.123 (external, sdk-cli)",
		"anthropic-version":        "2023-06-01",
		"content-length":           "1000",
		"x-claude-code-session-id": "11111111-1111-1111-1111-111111111111",
	})
	cand := snapshotFromHeaders(t, map[string]string{
		"user-agent":               "claude-cli/2.1.123 (external, sdk-cli)",
		"anthropic-version":        "2023-06-01",
		"content-length":           "999999",
		"x-claude-code-session-id": "22222222-2222-2222-2222-222222222222",
	})
	report := DiffSnapshots(ref, cand)
	if report.HasDiverged() {
		t.Fatalf("volatile-only difference diverged:\n%s", report.SummaryString())
	}
	cl := findFlavorHeader(t, ref, "content-length")
	if !cl.Volatile {
		t.Errorf("content-length not marked volatile")
	}
	if cl.ObservedValues != nil {
		t.Errorf("content-length stored observed values %v, want none", cl.ObservedValues)
	}
	sid := findFlavorHeader(t, ref, "x-claude-code-session-id")
	if !sid.Volatile {
		t.Errorf("x-claude-code-session-id not marked volatile")
	}
}

// TestStableHeaderDifferenceStillDiverges proves the detector is not too broad:
// a genuine identity header change (anthropic-version) still drives drift.
func TestStableHeaderDifferenceStillDiverges(t *testing.T) {
	t.Parallel()
	ref := snapshotFromHeaders(t, map[string]string{
		"user-agent":        "claude-cli/2.1.123 (external, sdk-cli)",
		"anthropic-version": "2023-06-01",
	})
	cand := snapshotFromHeaders(t, map[string]string{
		"user-agent":        "claude-cli/2.1.123 (external, sdk-cli)",
		"anthropic-version": "2024-10-22",
	})
	report := DiffSnapshots(ref, cand)
	if !report.HasDiverged() {
		t.Fatalf("stable header value change did not diverge; want drift")
	}
}
