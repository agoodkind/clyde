package capture

import "goodkind.io/clyde/internal/clock"

// RecordingEnabled reports whether the store accepts diagnostic exchanges.
// Baseline reads, learning, and retention operate independently of this policy.
func (s *Store) RecordingEnabled() bool {
	if s == nil || s.closed.Load() || s.cfg.RecordingDisabled {
		return false
	}
	if s.cfg.RecordingUntil.IsZero() {
		return true
	}
	return clock.Now().Before(s.cfg.RecordingUntil)
}
