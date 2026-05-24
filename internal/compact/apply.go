// Package compact implements append-only session compaction planning and apply.
package compact

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ApplyInput is the bundle the orchestrator hands to Apply once the
// preview has been rendered and the user opted in with --apply.
type ApplyInput struct {
	Slice         *Slice
	SessionID     string
	Cwd           string
	Version       string
	Strippers     Strippers
	Target        int
	BoundaryTail  []OutputBlock
	PreCompactTok int
	Options       SynthOptions

	// FinalProjection is the planner's converged projection. Recorded
	// for telemetry only; Apply does not gate on it. The bisect's
	// chosen k is the answer Apply commits.
	FinalProjection int
}

// ApplyResult summarises what apply did. Returned for preview and
// recorded in the ledger.
//
// In the natural-shape apply path, SyntheticUUID is the UUID of the
// prelude user entry (the small continuity-notice + dropped-stats
// message that follows the boundary). SyntheticLine is the file-line
// index of that prelude. SurvivorUUIDs lists the fresh UUIDs minted
// for the per-entry copies of slice.PostBoundary that follow the
// prelude. SurvivorLines is the number of survivor lines appended;
// the post-apply tail has 2+len(SurvivorUUIDs) new entries in total
// (boundary + prelude + survivors).
type ApplyResult struct {
	BoundaryUUID    string
	SyntheticUUID   string
	BoundaryLine    int
	SyntheticLine   int
	SurvivorUUIDs   []string
	SurvivorLines   int
	PreApplyOffset  int64
	PostApplyOffset int64
	SnapshotPath    string
	LedgerPath      string
	NoOp            bool
}

// Apply appends one compact_boundary system entry, one prelude user
// entry, and one entry per surviving post-boundary message to the
// JSONL transcript. The prelude carries the continuity notice plus the
// dropped-stats summary; the survivors are copies of slice.PostBoundary
// with the configured drops and transforms applied, chained via fresh
// parentUuid links so claude on resume reads them as natural messages
// after the boundary. A gzipped snapshot of the pre-Apply transcript
// and its sha256 are recorded in the ledger so --undo can restore the
// transcript byte-for-byte. The pre-Apply byte offset stays in the
// ledger entry for diagnostic value but is no longer used as a restore
// source (see CLYDE-375). The natural-shape append replaces the prior
// single-synthetic shape so the planner's projection matches the bytes
// Apply writes (see CLYDE-376).
func Apply(in ApplyInput) (*ApplyResult, error) {
	if in.Slice == nil {
		return nil, fmt.Errorf("apply: nil slice")
	}
	if in.SessionID == "" {
		return nil, fmt.Errorf("apply: empty session id")
	}
	if in.Target <= 0 {
		return nil, fmt.Errorf("apply: target must be greater than zero")
	}
	if in.FinalProjection > 0 {
		slog.Info("compact.apply.projection",
			"component", "compact",
			"subcomponent", "apply",
			"session_id", in.SessionID,
			"target", in.Target,
			"projection", in.FinalProjection,
			"delta", in.FinalProjection-in.Target,
		)
	}
	path := in.Slice.Path
	stat, err := os.Stat(path)
	if err != nil {
		slog.Error("compact.apply.stat_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("stat transcript: %w", err)
	}
	preOffset := stat.Size()

	if !hasNetApplyChange(in.Slice, in.Options) {
		slog.Info("compact.apply.noop",
			"component", "compact",
			"subcomponent", "apply",
			"session_id", in.SessionID,
			"target", in.Target,
		)
		return newNoOpApplyResult(preOffset), nil
	}

	snap, err := snapshotGzip(path, in.SessionID)
	if err != nil {
		slog.Error("compact.apply.snapshot_failed", "component", "compact", "path", path, "session_id", in.SessionID, "err", err)
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	parentUUID := lastChainUUID(in.Slice)
	now := compactClock.Now().UTC()
	entries, err := buildApplyEntries(in, parentUUID, now)
	if err != nil {
		return nil, err
	}

	in.Slice = Dehydrate(in.Slice, in.Options)
	if err := appendApplyEntries(path, entries); err != nil {
		return nil, err
	}

	postStat, err := os.Stat(path)
	if err != nil {
		slog.Error("compact.apply.post_stat_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("post-apply stat: %w", err)
	}
	if err := validateAppendedJSONL(path, preOffset); err != nil {
		slog.Error("compact.apply.validation_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("validate appended jsonl: %w", err)
	}

	res := newApplyResult(in.Slice, entries, preOffset, postStat.Size(), snap.Path)
	ledgerPath, err := appendLedger(in.SessionID, LedgerEntry{
		Timestamp:      now,
		Op:             "apply",
		Target:         in.Target,
		Strips:         strippersList(in.Strippers),
		PreApplyOffset: preOffset,
		PreApplySHA256: snap.SHA256Hex,
		SnapshotPath:   snap.Path,
		BoundaryUUID:   entries.BoundaryUUID,
		SyntheticUUID:  entries.PreludeUUID,
	})
	if err != nil {
		slog.Error("compact.apply.ledger_append_failed", "component", "compact", "session_id", in.SessionID, "err", err)
		return nil, fmt.Errorf("append ledger: %w", err)
	}
	res.LedgerPath = ledgerPath
	return res, nil
}

type applyEntries struct {
	BoundaryUUID  string
	PreludeUUID   string
	SurvivorUUIDs []string
	BoundaryLine  []byte
	PreludeLine   []byte
	SurvivorLines [][]byte
}

func buildApplyEntries(in ApplyInput, parentUUID string, now time.Time) (applyEntries, error) {
	boundaryUUID := uuid.NewString()
	boundaryLine, err := buildBoundaryEntry(boundaryEntryArgs{
		UUID:          boundaryUUID,
		ParentUUID:    parentUUID,
		SessionID:     in.SessionID,
		Cwd:           in.Cwd,
		Version:       in.Version,
		Timestamp:     now,
		PreCompactTok: in.PreCompactTok,
	})
	if err != nil {
		slog.Error("compact.apply.build_boundary_failed", "component", "compact", "session_id", in.SessionID, "err", err)
		return applyEntries{}, fmt.Errorf("build boundary: %w", err)
	}
	preludeUUID := uuid.NewString()
	preludeLine, err := MarshalPreludeEntry(preludeEntryArgs{
		UUID:       preludeUUID,
		ParentUUID: boundaryUUID,
		SessionID:  in.SessionID,
		Cwd:        in.Cwd,
		Version:    in.Version,
		Timestamp:  now.Add(time.Millisecond),
		Slice:      in.Slice,
		Options:    in.Options,
	})
	if err != nil {
		slog.Error("compact.apply.build_prelude_failed", "component", "compact", "session_id", in.SessionID, "err", err)
		return applyEntries{}, fmt.Errorf("build prelude: %w", err)
	}
	survivorLines, survivorUUIDs, _, err := buildSurvivorEntries(
		in.Slice,
		in.Options,
		preludeUUID,
		in.SessionID,
		in.Cwd,
		in.Version,
		now.Add(2*time.Millisecond),
	)
	if err != nil {
		slog.Error("compact.apply.build_survivors_failed", "component", "compact", "session_id", in.SessionID, "err", err)
		return applyEntries{}, fmt.Errorf("build survivors: %w", err)
	}
	return applyEntries{
		BoundaryUUID:  boundaryUUID,
		PreludeUUID:   preludeUUID,
		SurvivorUUIDs: survivorUUIDs,
		BoundaryLine:  boundaryLine,
		PreludeLine:   preludeLine,
		SurvivorLines: survivorLines,
	}, nil
}

func newNoOpApplyResult(offset int64) *ApplyResult {
	return &ApplyResult{
		BoundaryUUID:    "",
		SyntheticUUID:   "",
		BoundaryLine:    0,
		SyntheticLine:   0,
		SurvivorUUIDs:   nil,
		SurvivorLines:   0,
		PreApplyOffset:  offset,
		PostApplyOffset: offset,
		SnapshotPath:    "",
		LedgerPath:      "",
		NoOp:            true,
	}
}

func newApplyResult(
	slice *Slice,
	entries applyEntries,
	preOffset int64,
	postOffset int64,
	snapshotPath string,
) *ApplyResult {
	return &ApplyResult{
		BoundaryUUID:    entries.BoundaryUUID,
		SyntheticUUID:   entries.PreludeUUID,
		BoundaryLine:    len(slice.AllEntries),
		SyntheticLine:   len(slice.AllEntries) + 1,
		SurvivorUUIDs:   entries.SurvivorUUIDs,
		SurvivorLines:   len(entries.SurvivorLines),
		PreApplyOffset:  preOffset,
		PostApplyOffset: postOffset,
		SnapshotPath:    snapshotPath,
		LedgerPath:      "",
		NoOp:            false,
	}
}

func hasNetApplyChange(slice *Slice, opts SynthOptions) bool {
	if strings.TrimSpace(opts.Summary) != "" {
		return true
	}
	stats := ComputeDroppedStats(slice, opts)
	if stats.ThinkingBlocks > 0 ||
		stats.Images > 0 ||
		stats.ToolsLineOnly > 0 ||
		stats.ToolsDropped > 0 ||
		stats.ChatTurns > 0 {
		return true
	}
	for _, dropped := range opts.DroppedSummaryChunks {
		if len(dropped) > 0 {
			return true
		}
	}
	return false
}

// appendApplyEntries opens the transcript for append and writes the
// boundary, the prelude, and every survivor line followed by fsync.
// Extracted from Apply to keep the orchestrator function under the
// funlen ceiling.
func appendApplyEntries(path string, entries applyEntries) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Error("compact.apply.open_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("open transcript for append: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(entries.BoundaryLine, '\n')); err != nil {
		slog.Error("compact.apply.append_boundary_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("append boundary: %w", err)
	}
	if _, err := f.Write(append(entries.PreludeLine, '\n')); err != nil {
		slog.Error("compact.apply.append_prelude_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("append prelude: %w", err)
	}
	for index, line := range entries.SurvivorLines {
		if _, err := f.Write(append(line, '\n')); err != nil {
			slog.Error("compact.apply.append_survivor_failed",
				"component", "compact",
				"path", path,
				"survivor_index", index,
				"err", err,
			)
			return fmt.Errorf("append survivor %d: %w", index, err)
		}
	}
	if err := f.Sync(); err != nil {
		slog.Error("compact.apply.fsync_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// validateAppendedJSONL reads the bytes appended by Apply (from
// preOffset to EOF) and confirms the first line is the compact
// boundary and the second line is the prelude user entry carrying the
// isCompactSummary marker. Survivor lines after the prelude are not
// individually validated here; the appended writer's fsync plus the
// per-line JSON marshalling are the integrity guarantees there.
func validateAppendedJSONL(path string, preOffset int64) error {
	f, err := os.Open(path)
	if err != nil {
		slog.Error("compact.apply.validate_open_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(preOffset, io.SeekStart); err != nil {
		slog.Error("compact.apply.validate_seek_failed", "component", "compact", "path", path, "offset", preOffset, "err", err)
		return fmt.Errorf("seek transcript: %w", err)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 5*1024*1024)
	lines := make([]string, 0, 2)
	for len(lines) < 2 && scanner.Scan() {
		line := string(scanner.Bytes())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		slog.Error("compact.apply.validate_read_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("read transcript tail: %w", err)
	}
	if len(lines) < 2 {
		return fmt.Errorf("expected at least 2 appended lines, found %d", len(lines))
	}
	var boundary struct {
		Type           string `json:"type"`
		Subtype        string `json:"subtype"`
		CompactPayload struct {
			PreCompactTokenCount int `json:"preCompactTokenCount"`
		} `json:"compactMetadata"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &boundary); err != nil {
		slog.Error("compact.apply.validate_boundary_unmarshal_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("unmarshal boundary line: %w", err)
	}
	if boundary.Type != "system" || boundary.Subtype != "compact_boundary" {
		return fmt.Errorf("boundary line has unexpected type/subtype: %s/%s", boundary.Type, boundary.Subtype)
	}
	var prelude struct {
		Type           string `json:"type"`
		CompactSummary bool   `json:"isCompactSummary"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &prelude); err != nil {
		slog.Error("compact.apply.validate_prelude_unmarshal_failed", "component", "compact", "path", path, "err", err)
		return fmt.Errorf("unmarshal prelude line: %w", err)
	}
	if prelude.Type != "user" || !prelude.CompactSummary {
		return fmt.Errorf("prelude line missing compact summary marker")
	}
	if boundary.CompactPayload.PreCompactTokenCount < 0 {
		return fmt.Errorf("boundary pre-compact token count is invalid: %d", boundary.CompactPayload.PreCompactTokenCount)
	}
	return nil
}

func lastChainUUID(slice *Slice) string {
	for _, entry := range slices.Backward(slice.AllEntries) {
		if entry.UUID != "" {
			return entry.UUID
		}
	}
	return ""
}

func strippersList(s Strippers) []string {
	var out []string
	if s.Thinking {
		out = append(out, "thinking")
	}
	if s.Images {
		out = append(out, "images")
	}
	if s.Tools {
		out = append(out, "tools")
	}
	if s.Chat {
		out = append(out, "chat")
	}
	return out
}

type boundaryEntryArgs struct {
	UUID          string
	ParentUUID    string
	SessionID     string
	Cwd           string
	Version       string
	Timestamp     time.Time
	PreCompactTok int
}

// compactMetadata is the inner payload attached to every
// compact_boundary entry. clyde always emits trigger="manual"
// and surfaces the pre-compact token count for downstream tooling.
type compactMetadata struct {
	Trigger              string `json:"trigger"`
	PreCompactTokenCount int    `json:"preCompactTokenCount"`
}

// syntheticMessage is the inner `message` object on the synthetic
// user entry. Content holds the typed content array produced by
// synth.go.
type syntheticMessage struct {
	Role    string        `json:"role"`
	Content []OutputBlock `json:"content"`
}

// buildBoundaryEntry emits a system compact_boundary line. Field order
// places "compact_boundary" within the first 256 bytes so compatible
// large-file boundary scanners detect it.
func buildBoundaryEntry(a boundaryEntryArgs) ([]byte, error) {
	meta := compactMetadata{Trigger: "manual", PreCompactTokenCount: a.PreCompactTok}
	fields, err := orderedFields(
		field("parentUuid", optionalString(a.ParentUUID)),
		field("isSidechain", false),
		field("type", "system"),
		field("subtype", "compact_boundary"),
		field("content", "Conversation compacted by clyde."),
		field("isMeta", true),
		field("timestamp", a.Timestamp.Format(time.RFC3339Nano)),
		field("uuid", a.UUID),
		field("compactMetadata", meta),
		field("cwd", a.Cwd),
		field("sessionId", a.SessionID),
		field("version", a.Version),
	)
	if err != nil {
		return nil, err
	}
	return orderedJSON(fields).Marshal()
}

// orderedJSON is a minimal "ordered map" for JSON emission so we can
// control key order on disk. Each field carries a pre-encoded
// [json.RawMessage], so the ordered emitter never sees raw `any`: the
// type system enforces that callers serialize their values up front
// via mustField.
type orderedJSON []orderedJSONField

// orderedJSONField pairs a JSON object key with an already-encoded
// value. RawValue is JSON bytes ready to splice in verbatim, so the
// emitter is fully typed at the boundary.
type orderedJSONField struct {
	Key      string
	RawValue json.RawMessage
}

// jsonEncodable is the closed set of value types orderedJSONField
// accepts. Constrained to the concrete shapes apply.go and
// apply_natural.go actually emit so we never widen the surface to
// bare interface{}.
type jsonEncodable interface {
	string | bool | int | compactMetadata | syntheticMessage | survivorMessage | optionalString
}

// optionalString models a value that may be either a JSON string or
// JSON null. Empty string serializes as null; non-empty as the
// string. Used for parentUuid on the very first chain entry.
type optionalString string

// MarshalJSON makes optionalString satisfy [json.Marshaler] so it goes
// through the standard library encoder without an `any` detour.
func (o optionalString) MarshalJSON() ([]byte, error) {
	if o == "" {
		return []byte("null"), nil
	}
	encoded, err := json.Marshal(string(o))
	if err != nil {
		return nil, fmt.Errorf("optional string: marshal JSON: %w", err)
	}
	return encoded, nil
}

// pendingJSONField holds a typed key/value pair before encoding.
type pendingJSONField struct {
	Key      string
	RawValue json.RawMessage
	Err      error
}

// field pre-encodes a typed value and pairs it with key. The
// generic constraint means only the listed concrete types compile;
// any attempt to pass an interface or unknown struct is a build
// error rather than a runtime surprise.
func field[T jsonEncodable](key string, value T) pendingJSONField {
	encoded, err := json.Marshal(value)
	if err != nil {
		return pendingJSONField{Key: key, RawValue: nil, Err: fmt.Errorf("orderedJSON: marshal %q: %w", key, err)}
	}
	return pendingJSONField{Key: key, RawValue: encoded, Err: nil}
}

func orderedFields(fields ...pendingJSONField) ([]orderedJSONField, error) {
	out := make([]orderedJSONField, 0, len(fields))
	for _, f := range fields {
		if f.Err != nil {
			return nil, f.Err
		}
		out = append(out, orderedJSONField{Key: f.Key, RawValue: f.RawValue})
	}
	return out, nil
}

// Marshal concatenates the pre-encoded fields into one JSON object,
// preserving field order.
func (o orderedJSON) Marshal() ([]byte, error) {
	out := []byte{'{'}
	for i, f := range o {
		if i > 0 {
			out = append(out, ',')
		}
		key, err := json.Marshal(f.Key)
		if err != nil {
			slog.Error("compact.apply.ordered_json_key_failed", "component", "compact", "key", f.Key, "err", err)
			return nil, fmt.Errorf("orderedJSON: marshal key %q: %w", f.Key, err)
		}
		out = append(out, key...)
		out = append(out, ':')
		out = append(out, f.RawValue...)
	}
	out = append(out, '}')
	return out, nil
}
