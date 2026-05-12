package compact

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/contextusage"
)

// IterationEvent is the per-iteration JSON record streamed from the
// local planner. The shape is the contract Item 5 plumbs the
// matching slog event onto so JSON consumers and slog consumers can
// be correlated through CompactRunID.
type IterationEvent struct {
	Event        string `json:"event"`
	IterNum      int    `json:"iter_num"`
	Step         string `json:"step"`
	TailTokens   int    `json:"tail_tokens"`
	CtxTotal     int    `json:"ctx_total"`
	Projected    int    `json:"projected"`
	Delta        int    `json:"delta"`
	CompactRunID string `json:"compact_run_id,omitempty"`
}

// IsOutputPayload marks IterationEvent as a valid output.Encoder
// payload variant. The marker is empty by design.
func (IterationEvent) IsOutputPayload() {}

// ResultEvent is the terminal JSON record streamed from a target
// compact run. It reports the planner's final shape and whether the
// target was hit.
type ResultEvent struct {
	Event        string `json:"event"`
	HitTarget    bool   `json:"hit_target"`
	BaselineTail int    `json:"baseline_tail"`
	FinalTail    int    `json:"final_tail"`
	Target       int    `json:"target"`
	CompactRunID string `json:"compact_run_id,omitempty"`
}

// IsOutputPayload marks ResultEvent as a valid output.Encoder payload.
func (ResultEvent) IsOutputPayload() {}

// streamEvent is the closed union of per-line JSON events the local
// compact route emits. Both IterationEvent and ResultEvent satisfy
// it so emitStreamEvent stays parametric without resorting to any.
type streamEvent interface {
	IterationEvent | ResultEvent
	IsOutputPayload()
}

// emitStreamEvent writes one streamEvent as a single line of JSON
// followed by '\n'. Streaming callers invoke this once per record so
// the output is line-delimited JSON suitable for `jq -c` consumers.
func emitStreamEvent[E streamEvent](out io.Writer, ev E) error {
	encoded, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("compact.jsonout.marshal_failed",
			"component", "cli",
			"subcomponent", "compact",
			"err", err,
		)
		return fmt.Errorf("compact: marshal json event: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := out.Write(encoded); err != nil {
		slog.Warn("compact.jsonout.write_failed",
			"component", "cli",
			"subcomponent", "compact",
			"err", err,
		)
		return fmt.Errorf("compact: write json event: %w", err)
	}
	return nil
}

// MetricsDashboardJSON is the one-shot payload `clyde compact <name>`
// (no target) emits in JSON mode. The fields cover the read-only view
// the dashboard renders in text mode without changing semantics.
type MetricsDashboardJSON struct {
	Session               string                 `json:"session"`
	SessionID             string                 `json:"session_id"`
	Transcript            string                 `json:"transcript"`
	FileSizeBytes         int64                  `json:"file_size_bytes"`
	Calibrated            bool                   `json:"calibrated"`
	CalibrationOverhead   int                    `json:"calibration_overhead,omitempty"`
	CalibrationCapturedAt *time.Time             `json:"calibration_captured_at,omitempty"`
	Reserved              int                    `json:"reserved"`
	BoundaryLine          int                    `json:"boundary_line"`
	BoundaryUUID          string                 `json:"boundary_uuid,omitempty"`
	BoundaryTime          *time.Time             `json:"boundary_time,omitempty"`
	PostBoundaryEntries   int                    `json:"post_boundary_entries"`
	ThinkingBlocks        int                    `json:"thinking_blocks"`
	ImageBlocks           int                    `json:"image_blocks"`
	ToolPairs             int                    `json:"tool_pairs"`
	ChatTurns             int                    `json:"chat_turns"`
	Usage                 *contextusage.Snapshot `json:"usage,omitempty"`
	UsageError            string                 `json:"usage_error,omitempty"`
}

// IsOutputPayload marks MetricsDashboardJSON as a valid payload.
func (MetricsDashboardJSON) IsOutputPayload() {}

// LedgerListJSON is the array-shaped payload `--list-backups` emits
// in JSON mode. The slice is exposed directly so the top-level shape
// is a JSON array of entries rather than an object with a "ledger"
// key, matching how jq users expect to iterate the result.
type LedgerListJSON []LedgerEntryJSON

// IsOutputPayload marks LedgerListJSON as a valid payload.
func (LedgerListJSON) IsOutputPayload() {}

// LedgerEntryJSON is one row in LedgerListJSON. Field names are
// snake_case to match the project-wide JSON convention and the shape
// the text renderer prints in the dashboard.
type LedgerEntryJSON struct {
	Timestamp      time.Time `json:"timestamp"`
	Op             string    `json:"op"`
	Target         int       `json:"target"`
	Strips         []string  `json:"strips"`
	PreApplyOffset int64     `json:"pre_apply_offset"`
	SnapshotPath   string    `json:"snapshot_path"`
}

func newLedgerListJSON(entries []compactengine.LedgerEntry) LedgerListJSON {
	out := make(LedgerListJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, LedgerEntryJSON{
			Timestamp:      e.Timestamp.UTC(),
			Op:             e.Op,
			Target:         e.Target,
			Strips:         append([]string(nil), e.Strips...),
			PreApplyOffset: e.PreApplyOffset,
			SnapshotPath:   e.SnapshotPath,
		})
	}
	return out
}
