package mitm

import "time"

// DriftOutcome is one drift-check or baseline-refresh result. It is the typed
// return value the periodic loop and the refresher report on; the durable
// record of each run lives in the capture store's drift_checks table (cadence
// and the diverged flag) and baseline_diffs table (the per-difference matrix),
// not in any file.
type DriftOutcome struct {
	Timestamp time.Time   `json:"timestamp"`
	Upstream  string      `json:"upstream"`
	StartedAt time.Time   `json:"started_at"`
	Diverged  bool        `json:"diverged"`
	Summary   string      `json:"summary"`
	Report    *DiffReport `json:"report,omitempty"`
}
