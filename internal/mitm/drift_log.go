package mitm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/clock"
)

// driftSchemaVersion tags the snapshot schema the drift log records
// carry. The baseline uses the [[flavors]] table format.
type driftSchemaVersion string

const driftSchemaV2 driftSchemaVersion = "v2"

// DriftOutcome is one entry in the drift log. V2 carries the flavor
// diff when present. The TranscriptPath points at the JSONL capture
// that produced the candidate snapshot so the run can be reconstructed
// later.
type DriftOutcome struct {
	Timestamp      time.Time     `json:"timestamp"`
	Upstream       string        `json:"upstream"`
	SchemaVersion  string        `json:"schema_version"`
	ReferencePath  string        `json:"reference_path"`
	TranscriptPath string        `json:"transcript_path"`
	StartedAt      time.Time     `json:"started_at"`
	Diverged       bool          `json:"diverged"`
	Summary        string        `json:"summary"`
	V2             *DiffReportV2 `json:"v2,omitempty"`
}

// AppendDriftOutcome serializes the outcome and appends it to a
// JSONL log. Creates parent directories on first write. The Timestamp,
// Diverged, and Summary fields are populated automatically when zero.
func AppendDriftOutcome(path string, outcome DriftOutcome) error {
	if path == "" {
		return fmt.Errorf("drift log path is empty")
	}
	path = expandHome(path)
	if outcome.Timestamp.IsZero() {
		outcome.Timestamp = clock.Now().UTC()
	}
	if driftSchemaVersion(outcome.SchemaVersion) == driftSchemaV2 && outcome.V2 != nil {
		outcome.Diverged = outcome.V2.HasDiverged()
		outcome.Summary = outcome.V2.SummaryString()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("mitm.drift_log.mkdir_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("drift log mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("mitm.drift_log.open_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("drift log open: %w", err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	if err := enc.Encode(outcome); err != nil {
		slog.Warn("mitm.drift_log.encode_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("drift log encode: %w", err)
	}
	return nil
}
