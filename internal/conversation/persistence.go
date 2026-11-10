package conversation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (idx *Index) loadOnce() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.loaded {
		return nil
	}

	cache, present, err := readCache(idx.cachePath)
	if err != nil {
		return err
	}

	retained, err := readRetained(idx.retainedPath)
	if err != nil {
		return err
	}

	idx.records = cache.Records
	idx.retained = retained
	idx.prevRecords = recordsByPath(cache.Records)
	idx.prevStamps = cache.Stamps
	idx.prevMultiStates = cache.MultiStates
	idx.lastRefresh = cache.RefreshCompletedAt
	idx.cachePresent = present
	idx.loaded = true

	return nil
}

// recordsByPath indexes records by artifact path and selector so the next
// incremental scan can reuse every conversation from an unchanged artifact.
func recordsByPath(records []Record) map[string]Record {
	byPath := make(map[string]Record, len(records))
	for _, record := range records {
		byPath[recordKey(record.ArtifactPath, record.Selector)] = record
	}

	return byPath
}

func writeCache(
	path string,
	records []Record,
	stamps map[string]FileStamp,
	multiStates map[string]MultiConversationScanState,
	refreshCompletedAt time.Time,
) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn(
			"conversation.index.cache_mkdir_failed",
			"concern",
			"conversation.index",
			"component",
			"conversation",
			"path",
			filepath.Dir(path),
			"err",
			err,
		)

		return fmt.Errorf("create conversation cache dir: %w", err)
	}

	data, err := json.MarshalIndent(
		cacheFile{
			Version:            cacheFormatVersion,
			Records:            records,
			Stamps:             stamps,
			MultiStates:        multiStates,
			RefreshCompletedAt: refreshCompletedAt,
		},
		"",
		"  ",
	)
	if err != nil {
		slog.Warn(
			"conversation.index.cache_encode_failed",
			"concern",
			"conversation.index",
			"component",
			"conversation",
			"path",
			path,
			"err",
			err,
		)

		return fmt.Errorf("encode conversation cache: %w", err)
	}

	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		slog.Warn(
			"conversation.index.cache_write_failed",
			"concern",
			"conversation.index",
			"component",
			"conversation",
			"path",
			path,
			"err",
			err,
		)

		return fmt.Errorf("write conversation cache: %w", err)
	}

	slog.Debug(
		"conversation.index.cache_written",
		"concern",
		"conversation.index",
		"component",
		"conversation",
		"path",
		path,
		"records",
		len(records),
	)

	return nil
}

func sortRecords(records []Record) {
	sort.SliceStable(
		records,
		func(i int, j int) bool {
			left := records[i]
			right := records[j]
			if !left.UpdatedAt.Equal(right.UpdatedAt) {
				return left.UpdatedAt.After(right.UpdatedAt)
			}

			return left.ID < right.ID
		},
	)
}

func cloneRecords(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}

	out := make([]Record, len(records))
	copy(out, records)

	return out
}

func cloneStampedRecords(records []Record, stamps map[string]FileStamp) []StampedRecord {
	if len(records) == 0 {
		return nil
	}

	out := make([]StampedRecord, 0, len(records))
	for _, record := range records {
		out = append(
			out,
			StampedRecord{
				Record: record,
				Stamp:  stamps[recordKey(record.ArtifactPath, record.Selector)],
			},
		)
	}

	return out
}
