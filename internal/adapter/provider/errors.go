package provider

import "errors"

// ErrProviderNotRegistered is returned by Registry.Lookup when no
// provider has been registered for the given ProviderID. Dispatchers
// must surface this as a 5xx (server misconfiguration) rather than a
// 4xx (caller error).
var ErrProviderNotRegistered = errors.New("provider: not registered")

// ErrAuthMissing is returned by a Provider when AuthLookup yields no
// usable token. Dispatchers must surface this as a 401 to the
// upstream caller after recording the failure on the telemetry sink.
var ErrAuthMissing = errors.New("provider: auth missing")
