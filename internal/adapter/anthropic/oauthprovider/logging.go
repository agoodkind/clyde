package oauthprovider

import "goodkind.io/clyde/internal/slogger"

// providerLog is the concern logger for the Anthropic OAuth rotation plug-in.
// It shares the existing Anthropic OAuth concern so operators find rotation
// events alongside the legacy adapter OAuth events.
var providerLog = slogger.Concern(slogger.ConcernAdapterProviderAnthOAuth)
