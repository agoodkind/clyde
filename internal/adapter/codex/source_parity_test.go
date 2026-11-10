package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexRSMirrorRoot walks up from the package directory to locate the
// gitignored codex-rs research mirror. It returns ("", false) when the
// mirror is absent so the source-parity tests can skip on CI hosts that
// do not check the mirror out.
func codexRSMirrorRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, "research", "codex", "codex-rs")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func readMirrorFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read codex-rs mirror file %s: %v", rel, err)
	}
	return string(raw)
}

// TestCodexOriginatorMatchesSource guards that Clyde's spoofed originator
// still equals codex-rs DEFAULT_ORIGINATOR. If codex-cli changes the
// originator, this test fails so the constant gets re-pinned rather than
// silently drifting.
func TestCodexOriginatorMatchesSource(t *testing.T) {
	root, ok := codexRSMirrorRoot()
	if !ok {
		t.Skip("codex-rs mirror not present")
	}
	src := readMirrorFile(t, root, filepath.Join("login", "src", "auth", "default_client.rs"))
	want := `pub const DEFAULT_ORIGINATOR: &str = "` + CodexOriginatorValue + `";`
	if !strings.Contains(src, want) {
		t.Fatalf("codex-rs DEFAULT_ORIGINATOR no longer matches clyde CodexOriginatorValue=%q; expected source line %q", CodexOriginatorValue, want)
	}
}

// TestCodexUserAgentShapeMatchesSource guards that the codex-rs
// get_codex_user_agent format string still produces the
// "{originator}/{version} ({os} {ver}; {arch}) {terminal}" shape Clyde
// reproduces in CodexUserAgent.
func TestCodexUserAgentShapeMatchesSource(t *testing.T) {
	root, ok := codexRSMirrorRoot()
	if !ok {
		t.Skip("codex-rs mirror not present")
	}
	src := readMirrorFile(t, root, filepath.Join("login", "src", "auth", "default_client.rs"))
	// The format literal in get_codex_user_agent. If codex-rs reorders
	// the user-agent tokens, this fails so CodexUserAgent is updated.
	if !strings.Contains(src, `"{}/{build_version} ({} {}; {}) {}"`) {
		t.Fatalf("codex-rs get_codex_user_agent format string changed; re-verify CodexUserAgent shape")
	}
}

// TestCodexOpenAIBetaValueMatchesSource guards the responses-websocket
// beta header value against codex-rs.
func TestCodexOpenAIBetaValueMatchesSource(t *testing.T) {
	root, ok := codexRSMirrorRoot()
	if !ok {
		t.Skip("codex-rs mirror not present")
	}
	src := readMirrorFile(t, root, filepath.Join("core", "src", "client.rs"))
	want := `RESPONSES_WEBSOCKETS_V2_BETA_HEADER_VALUE: &str = "` + responsesWebsocketsV2BetaHeaderValue + `";`
	if !strings.Contains(src, want) {
		t.Fatalf("codex-rs RESPONSES_WEBSOCKETS_V2_BETA_HEADER_VALUE no longer matches clyde %q; expected %q", responsesWebsocketsV2BetaHeaderValue, want)
	}
}

// TestCodexHeaderNamesMatchSource guards the x-codex-* header name
// constants against codex-rs. Clyde stores these as canonical HTTP header
// names (X-Codex-...), which are case-insensitive matches for the
// lowercase codex-rs constants, so the comparison lowercases both sides.
func TestCodexHeaderNamesMatchSource(t *testing.T) {
	root, ok := codexRSMirrorRoot()
	if !ok {
		t.Skip("codex-rs mirror not present")
	}
	src := readMirrorFile(t, root, filepath.Join("core", "src", "client.rs"))
	cases := []struct {
		name        string
		clydeHeader string
	}{
		{"X_CODEX_INSTALLATION_ID_HEADER", CodexInstallationIDHeader},
		{"X_CODEX_TURN_STATE_HEADER", CodexTurnStateHeader},
		{"X_CODEX_TURN_METADATA_HEADER", CodexTurnMetadataHeader},
		{"X_CODEX_WINDOW_ID_HEADER", WindowIDHeader},
		{"X_RESPONSESAPI_INCLUDE_TIMING_METRICS_HEADER", CodexTimingMetricsHeader},
	}
	for _, tc := range cases {
		want := `"` + strings.ToLower(tc.clydeHeader) + `"`
		if !strings.Contains(src, want) {
			t.Fatalf("codex-rs %s no longer carries clyde header %q (lowercased %s); source drift", tc.name, tc.clydeHeader, want)
		}
	}
}
