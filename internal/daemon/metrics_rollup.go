package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"goodkind.io/clyde/internal/config"
)

// The rollup is a distilled, append-only sidecar to the daemon JSONL log.
//
// Reporting a window straight from the daemon log costs a full replay of every
// retained rotation, which is seconds of work and grows with log volume rather
// than with traffic. The status command reports three windows at once, so that
// replay would otherwise run on every invocation of a command that is
// currently instant.
//
// The rollup stores one record per finished request instead of one per log
// line, carrying only the fields the aggregation reads. That holds a week in a
// file the status command can scan whole, and it keeps exact percentiles
// because it stores per-request samples rather than pre-bucketed histograms.
// A separate generation record marks each daemon start, which is what lets a
// window spanning a restart report a summed total and a restart count.

const (
	// metricsRollupFileName is the append-only distilled request store.
	metricsRollupFileName = "metrics-rollup.jsonl"
	// metricsRollupCheckpointFileName records how far the writer has read.
	metricsRollupCheckpointFileName = "metrics-rollup-checkpoint.json"
	// metricsRollupRetention bounds the store. It exceeds the widest reported
	// window so a seven-day window stays fully covered while the daemon log
	// rotations behind it age out on their own schedule.
	metricsRollupRetention = 8 * 24 * time.Hour
	// maxMetricsRollupLineBytes bounds one rollup line. Records are
	// fixed-shape and small, so a longer line is corruption rather than data.
	maxMetricsRollupLineBytes = 64 * 1024
)

// metricsRollupKind names the record variants the store holds. The store is a
// union on the wire, so every record carries an explicit kind rather than
// being identified by whichever fields happen to be present.
type metricsRollupKind string

const (
	// metricsRollupKindRequest is one finished request.
	metricsRollupKindRequest metricsRollupKind = "request"
	// metricsRollupKindGeneration marks one daemon start.
	metricsRollupKindGeneration metricsRollupKind = "generation"
)

// metricsRollupRecord is one line of the rollup store.
//
// Timestamps are RFC3339 with nanoseconds so a reader reconstructs the same
// lifecycle instants the daemon log carried. StartedAt and StreamedAt are
// empty when the request never reached that stage, which the aggregation
// already treats as a different fact from a zero duration.
type metricsRollupRecord struct {
	Kind                metricsRollupKind    `json:"kind"`
	RequestID           string               `json:"request_id,omitempty"`
	StartedAt           string               `json:"started_at,omitempty"`
	StreamedAt          string               `json:"streamed_at,omitempty"`
	TerminalAt          string               `json:"terminal_at,omitempty"`
	Status              metricsRequestStatus `json:"status,omitempty"`
	Model               string               `json:"model,omitempty"`
	BytesIn             int64                `json:"bytes_in,omitempty"`
	BytesOut            int64                `json:"bytes_out,omitempty"`
	PromptTokens        int64                `json:"prompt_tokens,omitempty"`
	OutputTokens        int64                `json:"output_tokens,omitempty"`
	CacheReadTokens     int64                `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64                `json:"cache_creation_tokens,omitempty"`
	// GenerationStartedAt is the daemon start instant on a generation record.
	GenerationStartedAt string `json:"generation_started_at,omitempty"`
}

// metricsRollupCheckpoint records how much of the daemon log the writer has
// already distilled, so each pass reads only the new tail.
//
// The offset is meaningful only for the file named by Path. A rotation renames
// the active log, so a pass that finds a different path, or a file shorter than
// the checkpoint describes, restarts that file from the beginning rather than
// trusting a stale offset.
type metricsRollupCheckpoint struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	// LastRecordAt is the newest request terminal instant already written. It
	// suppresses duplicates when a file has to be re-read from the start.
	LastRecordAt string `json:"last_record_at"`
}

// metricsRollupPath returns the rollup store path inside the clyde state dir.
func metricsRollupPath() string {
	return filepath.Join(config.DefaultStateDir(), metricsRollupFileName)
}

// metricsRollupCheckpointPath returns the writer checkpoint path.
func metricsRollupCheckpointPath() string {
	return filepath.Join(config.DefaultStateDir(), metricsRollupCheckpointFileName)
}

// readMetricsRollupCheckpoint loads the writer checkpoint. A missing or
// unreadable checkpoint yields the zero value, which starts a full pass.
func readMetricsRollupCheckpoint(path string) metricsRollupCheckpoint {
	empty := metricsRollupCheckpoint{Path: "", Offset: 0, Size: 0, LastRecordAt: ""}
	raw, err := os.ReadFile(path) //nolint:gosec // daemon-owned state path
	if err != nil {
		return empty
	}
	var checkpoint metricsRollupCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return empty
	}
	return checkpoint
}

// writeMetricsRollupCheckpoint replaces the checkpoint atomically, so an
// interrupted write cannot leave a partial file that reads as offset zero.
func writeMetricsRollupCheckpoint(path string, checkpoint metricsRollupCheckpoint) error {
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("encode metrics rollup checkpoint: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create metrics rollup checkpoint directory: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("write metrics rollup checkpoint %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace metrics rollup checkpoint %s: %w", path, err)
	}
	return nil
}

// appendMetricsRollupRecords appends records to the store in one open.
func appendMetricsRollupRecords(path string, records []metricsRollupRecord) error {
	if len(records) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create metrics rollup directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // daemon-owned state path
	if err != nil {
		return fmt.Errorf("open metrics rollup %s: %w", path, err)
	}
	writer := bufio.NewWriter(file)
	for _, record := range records {
		encoded, encodeErr := json.Marshal(record)
		if encodeErr != nil {
			_ = file.Close()
			return fmt.Errorf("encode metrics rollup record: %w", encodeErr)
		}
		encoded = append(encoded, '\n')
		if _, writeErr := writer.Write(encoded); writeErr != nil {
			_ = file.Close()
			return fmt.Errorf("write metrics rollup record: %w", writeErr)
		}
	}
	if flushErr := writer.Flush(); flushErr != nil {
		_ = file.Close()
		return fmt.Errorf("flush metrics rollup %s: %w", path, flushErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close metrics rollup %s: %w", path, closeErr)
	}
	return nil
}

// metricsRollupWindow is one loaded window: the requests whose terminal instant
// falls inside it, and how many daemon generations started within it.
type metricsRollupWindow struct {
	Requests     map[string]*metricsRequest
	Restarts     int
	EarliestSeen time.Time
	Found        bool
}

// loadMetricsRollup reads the store once and buckets its records into every
// requested window. One pass serves all windows because the store is small and
// each record's instant decides which windows it belongs to.
//
// The returned slice is index-aligned with windows.
func loadMetricsRollup(path string, windows []MetricsWindow) ([]metricsRollupWindow, error) {
	loaded := make([]metricsRollupWindow, len(windows))
	for i := range loaded {
		loaded[i] = metricsRollupWindow{
			Requests:     make(map[string]*metricsRequest),
			Restarts:     0,
			EarliestSeen: time.Time{},
			Found:        false,
		}
	}
	file, err := os.Open(path) //nolint:gosec // daemon-owned state path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loaded, nil
		}
		return loaded, fmt.Errorf("open metrics rollup %s: %w", path, err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMetricsRollupLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record metricsRollupRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		applyRollupRecord(record, windows, loaded)
	}
	scanErr := scanner.Err()
	closeErr := file.Close()
	if scanErr != nil {
		return loaded, fmt.Errorf("read metrics rollup %s: %w", path, scanErr)
	}
	if closeErr != nil {
		return loaded, fmt.Errorf("close metrics rollup %s: %w", path, closeErr)
	}
	return loaded, nil
}

// applyRollupRecord folds one record into every window it belongs to.
func applyRollupRecord(record metricsRollupRecord, windows []MetricsWindow, loaded []metricsRollupWindow) {
	switch record.Kind {
	case metricsRollupKindGeneration:
		startedAt, ok := parseRollupTime(record.GenerationStartedAt)
		if !ok {
			return
		}
		for i, window := range windows {
			markRollupEarliest(&loaded[i], startedAt)
			if startedAt.After(window.Since) && !startedAt.After(window.Until) {
				loaded[i].Restarts++
			}
		}
	case metricsRollupKindRequest:
		terminalAt, ok := parseRollupTime(record.TerminalAt)
		if !ok {
			return
		}
		request := rollupRecordToRequest(record, terminalAt)
		for i, window := range windows {
			markRollupEarliest(&loaded[i], terminalAt)
			if terminalAt.Before(window.Since) || terminalAt.After(window.Until) {
				continue
			}
			loaded[i].Requests[record.RequestID] = request
			loaded[i].Found = true
		}
	default:
	}
}

// markRollupEarliest tracks the oldest instant the store holds, which is how
// the reader decides whether retained history brackets the window start.
func markRollupEarliest(window *metricsRollupWindow, at time.Time) {
	if window.EarliestSeen.IsZero() || at.Before(window.EarliestSeen) {
		window.EarliestSeen = at
	}
}

// rollupRecordToRequest rebuilds the aggregation's request shape from a record.
// The aggregation is shared with the log-replay path, so the rollup produces
// that same struct rather than a parallel summary type.
func rollupRecordToRequest(record metricsRollupRecord, terminalAt time.Time) *metricsRequest {
	startedAt, hasStarted := parseRollupTime(record.StartedAt)
	streamedAt, _ := parseRollupTime(record.StreamedAt)
	return &metricsRequest{
		started:             hasStarted,
		terminal:            true,
		lifecycleStarted:    hasStarted,
		lifecycleTerminal:   true,
		invalidLifecycle:    false,
		status:              record.Status,
		totalMS:             0,
		bytesIn:             record.BytesIn,
		bytesOut:            record.BytesOut,
		promptTokens:        record.PromptTokens,
		outputTokens:        record.OutputTokens,
		cacheTokens:         record.CacheReadTokens + record.CacheCreationTokens,
		cacheReadTokens:     record.CacheReadTokens,
		cacheCreationTokens: record.CacheCreationTokens,
		model:               record.Model,
		ioSeen:              record.BytesIn != 0 || record.BytesOut != 0,
		startedAt:           startedAt,
		streamedAt:          streamedAt,
		terminalAt:          terminalAt,
		legDurations:        nil,
	}
}

// parseRollupTime parses a stored instant. An empty or malformed value reports
// absence rather than the zero instant, so a caller cannot mistake a stage that
// never happened for one that happened at the epoch.
func parseRollupTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// formatRollupTime renders an instant for the store, or the empty string when
// the stage never happened.
func formatRollupTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// requestToRollupRecord distills one aggregated request into a stored record.
func requestToRollupRecord(requestID string, request *metricsRequest) metricsRollupRecord {
	return metricsRollupRecord{
		Kind:                metricsRollupKindRequest,
		RequestID:           requestID,
		StartedAt:           formatRollupTime(request.startedAt),
		StreamedAt:          formatRollupTime(request.streamedAt),
		TerminalAt:          formatRollupTime(request.terminalAt),
		Status:              request.status,
		Model:               request.model,
		BytesIn:             request.bytesIn,
		BytesOut:            request.bytesOut,
		PromptTokens:        request.promptTokens,
		OutputTokens:        request.outputTokens,
		CacheReadTokens:     request.cacheReadTokens,
		CacheCreationTokens: request.cacheCreationTokens,
		GenerationStartedAt: "",
	}
}

// generationRollupRecord builds the marker written when a daemon generation
// starts, which is what a window uses to report restarts.
func generationRollupRecord(startedAt time.Time) metricsRollupRecord {
	return metricsRollupRecord{
		Kind:                metricsRollupKindGeneration,
		RequestID:           "",
		StartedAt:           "",
		StreamedAt:          "",
		TerminalAt:          "",
		Status:              "",
		Model:               "",
		BytesIn:             0,
		BytesOut:            0,
		PromptTokens:        0,
		OutputTokens:        0,
		CacheReadTokens:     0,
		CacheCreationTokens: 0,
		GenerationStartedAt: formatRollupTime(startedAt),
	}
}

// pruneMetricsRollup rewrites the store without records older than the cutoff
// and returns how many it dropped.
//
// The rewrite goes through a temporary file and one rename, so a concurrent
// reader sees either the whole old store or the whole new one.
func pruneMetricsRollup(path string, cutoff time.Time) (int, error) {
	file, err := os.Open(path) //nolint:gosec // daemon-owned state path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("open metrics rollup %s: %w", path, err)
	}
	kept := make([]string, 0, 1024)
	dropped := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMetricsRollupLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if rollupLineBefore(line, cutoff) {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	scanErr := scanner.Err()
	closeErr := file.Close()
	if scanErr != nil {
		return 0, fmt.Errorf("read metrics rollup %s: %w", path, scanErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close metrics rollup %s: %w", path, closeErr)
	}
	if dropped == 0 {
		return 0, nil
	}
	payload := make([]byte, 0, len(kept)*128)
	for _, line := range kept {
		payload = append(payload, line...)
		payload = append(payload, '\n')
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return 0, fmt.Errorf("write pruned metrics rollup %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return 0, fmt.Errorf("replace metrics rollup %s: %w", path, err)
	}
	return dropped, nil
}

// rollupLineBefore reports whether a stored line is older than the cutoff. An
// unparsable line is kept, because dropping unreadable data would silently
// shrink the window a reader believes it has.
func rollupLineBefore(line string, cutoff time.Time) bool {
	var record metricsRollupRecord
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return false
	}
	at, ok := parseRollupTime(rollupRecordStamp(record))
	if !ok {
		return false
	}
	return at.Before(cutoff)
}

// sortedRollupRecords orders records by their stored instant, so the store
// stays append-ordered even when a pass emits requests from a map.
func sortedRollupRecords(records []metricsRollupRecord) []metricsRollupRecord {
	slices.SortFunc(records, func(a, b metricsRollupRecord) int {
		left, _ := parseRollupTime(rollupRecordStamp(a))
		right, _ := parseRollupTime(rollupRecordStamp(b))
		return left.Compare(right)
	})
	return records
}

// rollupRecordStamp returns the instant that orders a record.
func rollupRecordStamp(record metricsRollupRecord) string {
	if record.Kind == metricsRollupKindGeneration {
		return record.GenerationStartedAt
	}
	return record.TerminalAt
}
