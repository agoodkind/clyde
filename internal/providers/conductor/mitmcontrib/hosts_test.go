package mitmcontrib

import "testing"

func TestIsConductorHostClaimsKnownHosts(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"app.conductor.build",
		"api.conductor.build",
		"storage.conductor.build",
		"conductor.build",
		"conductor-roundhouse.fly.dev",
		"cdn.crabnebula.app",
		"api.github.com",
		"api.linear.app",
		"app.chorus.sh",
		"sb-x.vercel.run",
		"us.i.posthog.com",
		"api.honeycomb.io",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			if !isConductorHost(host) {
				t.Fatalf("isConductorHost(%q) = false, want true", host)
			}
		})
	}
}

func TestIsConductorHostLeavesUnrelatedHostsOpaque(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"example.com",
		"api.anthropic.com",
		"github.com",
		"some-other.fly.dev",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			if isConductorHost(host) {
				t.Fatalf("isConductorHost(%q) = true, want false", host)
			}
		})
	}
}
