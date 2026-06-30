package loginventory

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// inventoryIndexSnapshot summarizes the most recent entries in
// `logs/inventory/events.jsonl` for indexed-mode discovery. It maps
// each sink to the most recent event timestamp and request_id seen in
// the index, and exposes the most recent cleanup completion as a typed
// summary. The snapshot is purely additive over the deep filesystem
// view and is never used when callers ask for `--mode deep`.
type inventoryIndexSnapshot struct {
	LastEventBySink map[string]inventoryEventSummary
	LastCleanup     *inventoryCleanupSummary
}

// inventoryEventSummary records the timestamp and request_id of the
// most recent event written to the inventory index for a given sink.
type inventoryEventSummary struct {
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id,omitempty"`
}

// inventoryCleanupSummary is the typed projection of a
// `slogger.cleanup.completed` event written to the inventory index.
// Fields mirror the contract documented in
// docs/logging/rotation-cleanup.md. Unset slices serialise as null
// rather than empty arrays to make missing data explicit.
type inventoryCleanupSummary struct {
	Timestamp    time.Time `json:"timestamp"`
	Root         string    `json:"root,omitempty"`
	ScannedRoots []string  `json:"scanned_roots,omitempty"`
	Candidates   int       `json:"candidates"`
	Deleted      int       `json:"deleted"`
	BytesDeleted int64     `json:"bytes_deleted"`
	Skipped      []string  `json:"skipped,omitempty"`
	Errors       []string  `json:"errors,omitempty"`
	DurationMS   int64     `json:"duration_ms"`
}

// InventoryCleanupSummary is the exported alias for the indexed cleanup event
// summary embedded in category summaries.
type InventoryCleanupSummary = inventoryCleanupSummary

// inventoryIndexEventRecord is the typed shape used to decode lines of
// the inventory index file. It is intentionally narrow: only fields
// the snapshot reader actually consumes appear here, so unrelated slog
// attrs are ignored without resorting to map[string]any. The Time field
// uses the slog default key "time".
type inventoryIndexEventRecord struct {
	Time         time.Time `json:"time"`
	Msg          string    `json:"msg"`
	Sinks        []string  `json:"sinks"`
	RequestID    string    `json:"request_id"`
	Root         string    `json:"root"`
	ScannedRoots []string  `json:"scanned_roots"`
	Candidates   int       `json:"candidates"`
	Deleted      int       `json:"deleted"`
	BytesDeleted int64     `json:"bytes_deleted"`
	Skipped      []string  `json:"skipped"`
	Errors       []string  `json:"errors"`
	DurationMS   int64     `json:"duration_ms"`
}

const (
	cleanupCompletedEvent = "slogger.cleanup.completed"
	inventoryIndexPath    = "logs/inventory/events.jsonl"
)

// readInventoryIndexSnapshot parses every JSON line in
// `<stateRoot>/logs/inventory/events.jsonl` and returns the resulting
// snapshot. A missing file is treated as an empty snapshot. The
// snapshot scan stops on the first non-recoverable read error and
// keeps whatever data it has already accumulated. The returned
// snapshot has a non-nil map even when the index file is absent so
// downstream callers can use plain map indexing without nil checks.
func readInventoryIndexSnapshot(stateRoot string) inventoryIndexSnapshot {
	snapshot := inventoryIndexSnapshot{
		LastEventBySink: make(map[string]inventoryEventSummary),
		LastCleanup:     nil,
	}
	path := filepath.Join(stateRoot, inventoryIndexPath)
	file, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn(
				"cli.logs.inventory.index_open_failed", "concern", "cli.logs", "component", "cli",
				"path", path,
				"err", err,
			)
		}
		return snapshot
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn(
				"cli.logs.inventory.index_close_failed", "concern", "cli.logs", "component", "cli",
				"path", path,
				"err", closeErr,
			)
		}
	}()
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			applyInventoryIndexLine(&snapshot, line)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				slog.Warn(
					"cli.logs.inventory.index_read_failed", "concern", "cli.logs", "component", "cli",
					"path", path,
					"err", readErr,
				)
			}
			break
		}
	}
	return snapshot
}

func applyInventoryIndexLine(snapshot *inventoryIndexSnapshot, line []byte) {
	trimmed := dropTrailingNewline(line)
	if len(trimmed) == 0 {
		return
	}
	record := inventoryIndexEventRecord{
		Time:         time.Time{},
		Msg:          "",
		Sinks:        nil,
		RequestID:    "",
		Root:         "",
		ScannedRoots: nil,
		Candidates:   0,
		Deleted:      0,
		BytesDeleted: 0,
		Skipped:      nil,
		Errors:       nil,
		DurationMS:   0,
	}
	if err := json.Unmarshal(trimmed, &record); err != nil {
		return
	}
	if record.Time.IsZero() {
		return
	}
	for _, sinkName := range record.Sinks {
		if sinkName == "" {
			continue
		}
		previous, exists := snapshot.LastEventBySink[sinkName]
		if exists && !record.Time.After(previous.Timestamp) {
			continue
		}
		snapshot.LastEventBySink[sinkName] = inventoryEventSummary{
			Timestamp: record.Time,
			RequestID: record.RequestID,
		}
	}
	if record.Msg == cleanupCompletedEvent {
		if snapshot.LastCleanup != nil && !record.Time.After(snapshot.LastCleanup.Timestamp) {
			return
		}
		summary := cleanupSummaryFromRecord(record)
		snapshot.LastCleanup = &summary
	}
}

func cleanupSummaryFromRecord(record inventoryIndexEventRecord) inventoryCleanupSummary {
	return inventoryCleanupSummary{
		Timestamp:    record.Time,
		Root:         record.Root,
		ScannedRoots: cloneStringSlice(record.ScannedRoots),
		Candidates:   record.Candidates,
		Deleted:      record.Deleted,
		BytesDeleted: record.BytesDeleted,
		Skipped:      cloneStringSlice(record.Skipped),
		Errors:       cloneStringSlice(record.Errors),
		DurationMS:   record.DurationMS,
	}
}

func cloneStringSlice(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	output := make([]string, len(input))
	copy(output, input)
	return output
}

func dropTrailingNewline(line []byte) []byte {
	end := len(line)
	for end > 0 {
		last := line[end-1]
		if last != '\n' && last != '\r' {
			break
		}
		end--
	}
	return line[:end]
}
