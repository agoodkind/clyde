package codex

import "goodkind.io/clyde/internal/slogger"

var (
	codexConcernLog     = slogger.Concern(slogger.ConcernAdapterProviderCodex)
	codexWireCaptureLog = slogger.Concern(slogger.ConcernAdapterProviderCodexWire)
)
