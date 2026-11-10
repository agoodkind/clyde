package mitm

import "testing"

// TestAutoClassifyConcernDerivesProviderBuckets locks the fallback behavior:
// when no curated rule matches, the concern is provider-prefixed and derived
// from the request path, never a bare "unknown" unless the provider is unknown.
func TestAutoClassifyConcernDerivesProviderBuckets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider string
		path     string
		want     string
	}{
		{name: "grpc service name", provider: "cursor", path: "/aiserver.v1.AnalyticsService/Batch", want: "cursor.analytics"},
		{name: "grpc dashboard", provider: "cursor", path: "/aiserver.v1.DashboardService/GetTeams", want: "cursor.dashboard"},
		{name: "claude messages", provider: "claude", path: "/v1/messages", want: "claude.messages"},
		{name: "codex backend segment", provider: "codex", path: "/backend-api/codex/remote/control/environments", want: "codex.remote"},
		{name: "codex telemetry skips ces and version", provider: "codex", path: "/ces/v1/telemetry/intake", want: "codex.telemetry"},
		{name: "cursor auth segment", provider: "cursor", path: "/auth/full_stripe_profile", want: "cursor.auth"},
		{name: "root path falls back to provider other", provider: "claude", path: "/", want: "claude.other"},
		{name: "version-only path falls back", provider: "codex", path: "/v1", want: "codex.other"},
		{name: "id-like segment skipped", provider: "claude", path: "/api/organizations/c11a740ab5dd4c7792b9", want: "claude.organizations"},
		{name: "unknown provider stays unknown", provider: "", path: "/whatever", want: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := autoClassifyConcern(captureConcernInput{Provider: tc.provider, Path: tc.path})
			if got != tc.want {
				t.Fatalf("autoClassifyConcern(%q, %q) = %q, want %q", tc.provider, tc.path, got, tc.want)
			}
		})
	}
}

// TestResolveCaptureConcernPrefersCuratedRuleThenAutoclassifies asserts a
// curated rule still wins over autoclassification, and a non-matching request
// falls through to the derived bucket.
func TestResolveCaptureConcernPrefersCuratedRuleThenAutoclassifies(t *testing.T) {
	t.Parallel()

	curated := resolveCaptureConcern(nil, captureConcernInput{
		Provider: "cursor",
		Method:   "POST",
		Path:     "/aiserver.v1.AiService/BidiAppend",
	})
	if curated != "cursor.bidi" {
		t.Fatalf("curated rule concern = %q, want cursor.bidi", curated)
	}

	derived := resolveCaptureConcern(nil, captureConcernInput{
		Provider: "claude",
		Method:   "POST",
		Path:     "/v1/messages",
	})
	if derived != "claude.messages" {
		t.Fatalf("autoclassified concern = %q, want claude.messages", derived)
	}
}
