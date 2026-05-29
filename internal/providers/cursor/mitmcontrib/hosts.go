// Package mitmcontrib registers the Cursor provider with the MITM
// proxy registry. The package owns every Cursor-specific shape the
// generic MITM layer would otherwise have to bake in: the
// CONNECT-host whitelist, the identity-header schema, the
// capture-event wire format, and the BidiAppend protobuf diagnostic.
// internal/mitm calls into this contract through the typed
// [goodkind.io/clyde/internal/mitm.Provider] interface; it never
// imports this package directly.
package mitmcontrib

import (
	"strings"
)

// ProviderName is the stable identifier the generic MITM layer and
// downstream concern routing use for Cursor traffic. The value also
// flows through the per-provider concern path under
// `mitm/capture.jsonl` and the typed `provider` field on emitted
// events.
const ProviderName = "cursor"

// serviceHost enumerates the well-known Cursor backend hosts that
// always claim TLS interception. Hosts outside this set fall through
// to the suffix check below.
type serviceHost string

const (
	api2Host serviceHost = "api2.cursor.sh"
	api3Host serviceHost = "api3.cursor.sh"
)

func isServiceHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), ".")
	switch serviceHost(host) {
	case api2Host, api3Host:
		return true
	}
	return strings.HasSuffix(host, ".cursor.sh") ||
		strings.HasSuffix(host, ".cursor.com")
}
