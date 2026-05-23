package adapter

import "goodkind.io/clyde/internal/config"

// RuntimeLogging exposes daemon-owned logging knobs that can change without
// reconstructing the adapter server. No payload-mode knobs remain after the
// unified logging hard cut, so this holder is intentionally a narrow reload
// coordination point.
type RuntimeLogging struct{}

// NewRuntimeLogging returns a runtime logging holder seeded from cfg.
func NewRuntimeLogging(_ config.LoggingConfig) *RuntimeLogging {
	return &RuntimeLogging{}
}

// Set publishes the current runtime logging settings.
func (r *RuntimeLogging) Set(_ config.LoggingConfig) {
	if r == nil {
		return
	}
}
