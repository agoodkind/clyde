package vectorsearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CopyConversationCheckpoints copies the source collection's fingerprint
// records to the target collection under checkpointDir without using Milvus.
// It returns the number of copied records.
func CopyConversationCheckpoints(checkpointDir string, sourceID string, targetID string) (int, error) {
	store := newCheckpointStore(strings.TrimSpace(checkpointDir))
	return copyCheckpoints(store, sourceID, targetID)
}

// CopySharedCheckpoints copies the source collection's fingerprint records
// to the target collection and replaces the target's in-memory records.
// It returns the number of copied records.
func (c *Client) CopySharedCheckpoints(sourceID string, targetID string) (int, error) {
	if c == nil {
		return 0, errors.New("copy conversation fingerprint records: client is nil")
	}
	return copyCheckpoints(c.checkpoint, sourceID, targetID)
}

func copyCheckpoints(store *checkpointStore, sourceID string, targetID string) (int, error) {
	trimmedSourceID := strings.TrimSpace(sourceID)
	trimmedTargetID := strings.TrimSpace(targetID)
	if trimmedSourceID == "" || trimmedTargetID == "" {
		return 0, errors.New("copy conversation fingerprint records: source and target collection ids are required")
	}
	source := CollectionName(trimmedSourceID)
	target := CollectionName(trimmedTargetID)
	if source == target {
		return 0, fmt.Errorf("copy conversation fingerprint records: source and target are both %s", source)
	}
	copied, err := store.copyCollection(source, target)
	if err != nil {
		return 0, err
	}
	slog.Info("conversation.vectorsearch.checkpoint_copied",
		"concern", "conversation.semantic",
		"component", "conversation",
		"source", source,
		"target", target,
		"records", copied,
	)
	return copied, nil
}

// copyCollection reads the source records, atomically writes their saved values
// to the target, and removes target record files absent from the source.
// It reads the target again and fails unless its records match the source
// exactly. A missing source directory fails the copy before the target changes.
// The method does not change the source directory.
func (store *checkpointStore) copyCollection(source string, target string) (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.root == "" {
		return 0, errors.New("the checkpoint directory is not configured")
	}
	sourceDir := filepath.Join(store.root, source)
	targetDir := filepath.Join(store.root, target)
	operation := "copy conversation fingerprint records from " + source + " to " + target
	// readCheckpointDir and writeCheckpointFile log their own failures.
	// copyCollection wraps those errors without logging them again and logs only
	// failures that start in copyCollection.
	sourceRecords, _, err := readCheckpointDir(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		missing := fmt.Errorf("%s: source checkpoint directory %s does not exist: %w", operation, sourceDir, err)
		slog.Warn("conversation.vectorsearch.checkpoint_copy_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", missing,
		)
		return 0, missing
	}
	if err != nil {
		return 0, fmt.Errorf("%s: read source records: %w", operation, err)
	}
	ids := slices.Sorted(maps.Keys(sourceRecords))
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := writeCheckpointFile(targetDir, id, sourceRecords[id]); err != nil {
			return 0, fmt.Errorf("%s: %w", operation, err)
		}
		keep[checkpointFileName(id)] = struct{}{}
	}
	if err := os.MkdirAll(targetDir, checkpointDirMode); err != nil {
		failure := fmt.Errorf("%s: create checkpoint directory %s: %w", operation, targetDir, err)
		slog.Warn("conversation.vectorsearch.checkpoint_copy_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", failure,
		)
		return 0, failure
	}
	_, targetNames, err := readCheckpointDir(targetDir)
	if err != nil {
		return 0, fmt.Errorf("%s: read target records: %w", operation, err)
	}
	for _, name := range targetNames {
		if _, found := keep[name]; found {
			continue
		}
		if err := os.Remove(filepath.Join(targetDir, name)); err != nil {
			failure := fmt.Errorf("%s: remove target record %s: %w", operation, name, err)
			slog.Warn("conversation.vectorsearch.checkpoint_copy_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"err", failure,
			)
			return 0, failure
		}
	}
	written, _, err := readCheckpointDir(targetDir)
	if err != nil {
		return 0, fmt.Errorf("%s: read target records after the copy: %w", operation, err)
	}
	if !maps.Equal(written, sourceRecords) {
		failure := fmt.Errorf("%s: target directory %s stores %d records after the copy, want the %d source records", operation, targetDir, len(written), len(sourceRecords))
		slog.Warn("conversation.vectorsearch.checkpoint_copy_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", failure,
		)
		return 0, failure
	}
	store.loaded[target] = maps.Clone(sourceRecords)
	return len(sourceRecords), nil
}

// readCheckpointDir returns a directory's records by conversation ID and its
// record file names. It fails on unreadable or invalid files and records without
// a conversation ID. It logs failures except for a missing directory.
func readCheckpointDir(directory string) (records map[string]string, names []string, failure error) {
	defer func() {
		if failure != nil && !errors.Is(failure, os.ErrNotExist) {
			slog.Warn("conversation.vectorsearch.checkpoint_dir_strict_read_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"path", directory,
				"err", failure,
			)
		}
	}()
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("read checkpoint directory %s: %w", directory, err)
	}
	records = make(map[string]string, len(entries))
	names = make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), checkpointFileSuffix) || strings.HasPrefix(entry.Name(), checkpointTempPrefix) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read checkpoint record %s: %w", path, readErr)
		}
		var record checkpointRecord
		if decodeErr := json.Unmarshal(content, &record); decodeErr != nil {
			return nil, nil, fmt.Errorf("decode checkpoint record %s: %w", path, decodeErr)
		}
		if record.ConversationID == "" {
			return nil, nil, fmt.Errorf("checkpoint record %s has no conversation id", path)
		}
		records[record.ConversationID] = record.Fingerprint
		names = append(names, entry.Name())
	}
	return records, names, nil
}
