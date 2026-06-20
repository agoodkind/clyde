// Package mitmcontrib registers the Conductor provider with the MITM
// proxy registry.
package mitmcontrib

import "strings"

func isConductorHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), ".")

	// First-party hosts.
	if host == "conductor.build" || strings.HasSuffix(host, ".conductor.build") {
		return true
	}
	if host == "conductor-roundhouse.fly.dev" || host == "conductor-cloud-alpha.fly.dev" {
		return true
	}
	if host == "cdn.crabnebula.app" || strings.HasSuffix(host, ".crabnebula.app") {
		return true
	}

	// Integration hosts.
	if host == "api.github.com" || host == "api.linear.app" || host == "app.chorus.sh" {
		return true
	}
	if strings.HasSuffix(host, ".vercel.run") {
		return true
	}

	// Telemetry hosts.
	if host == "us.i.posthog.com" || strings.HasSuffix(host, ".posthog.com") {
		return true
	}
	return host == "api.honeycomb.io" || strings.HasSuffix(host, ".honeycomb.io")
}
