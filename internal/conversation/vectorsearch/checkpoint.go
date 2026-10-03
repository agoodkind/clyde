package vectorsearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const (
	checkpointFileSuffix = ".json"
	checkpointTempPrefix = ".tmp-"
	checkpointDirMode    = 0o700
	// checkpointNameLength is the hex length of a record file name, taken from the
	// SHA-256 of the conversation ID.
	checkpointNameLength = 32
)

// checkpointRecord is the file content of one conversation's fingerprint record.
type checkpointRecord struct {
	ConversationID string `json:"conversation_id"`
	Fingerprint    string `json:"fingerprint"`
}

// checkpointStore keeps the manifest fingerprint of each conversation after its
// rows are written. A record is one file under <root>/<collection>/, written to
// a temporary file and renamed into place. The store loads a collection's
// records on first use. An empty root keeps the records in memory only.
type checkpointStore struct {
	root   string
	mu     sync.Mutex
	loaded map[string]map[string]string
}

func newCheckpointStore(root string) *checkpointStore {
	return &checkpointStore{root: root, mu: sync.Mutex{}, loaded: make(map[string]map[string]string)}
}

// records returns the fingerprints of a collection and loads its record files on
// first use. The caller must hold the lock. A file that cannot be read or
// decoded is skipped, and its conversation reads as unrecorded.
func (store *checkpointStore) records(collectionName string) map[string]string {
	if fingerprints, found := store.loaded[collectionName]; found {
		return fingerprints
	}
	fingerprints := make(map[string]string)
	store.loaded[collectionName] = fingerprints
	if store.root == "" {
		return fingerprints
	}
	directory := filepath.Join(store.root, collectionName)
	entries, err := os.ReadDir(directory)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("conversation.vectorsearch.checkpoint_dir_read_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"path", directory,
				"err", err,
			)
		}
		return fingerprints
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), checkpointFileSuffix) || strings.HasPrefix(entry.Name(), checkpointTempPrefix) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			slog.Warn("conversation.vectorsearch.checkpoint_read_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"path", path,
				"err", readErr,
			)
			continue
		}
		var record checkpointRecord
		if decodeErr := json.Unmarshal(content, &record); decodeErr != nil || record.ConversationID == "" {
			slog.Warn("conversation.vectorsearch.checkpoint_decode_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"path", path,
				"err", decodeErr,
			)
			continue
		}
		fingerprints[record.ConversationID] = record.Fingerprint
	}
	return fingerprints
}

// needed returns the sorted IDs of the manifest conversations with no record or
// a record with a different fingerprint.
func (store *checkpointStore) needed(collectionName string, ids []string, fingerprints []string) []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	recorded := store.records(collectionName)
	needed := make([]string, 0)
	for i, id := range ids {
		if id == "" {
			continue
		}
		if value, found := recorded[id]; found && value == fingerprints[i] {
			continue
		}
		needed = append(needed, id)
	}
	slices.Sort(needed)
	return slices.Compact(needed)
}

// record stores one conversation's fingerprint. It writes the file before it
// updates the memory copy. writeCheckpointFile logs a failed write. After a
// failed write the memory copy is not updated, and the conversation stays
// needed after a restart.
func (store *checkpointStore) record(collectionName string, conversationID string, fingerprint string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	recorded := store.records(collectionName)
	if store.root != "" {
		if err := writeCheckpointFile(filepath.Join(store.root, collectionName), conversationID, fingerprint); err != nil {
			return
		}
	}
	recorded[conversationID] = fingerprint
}

// writeCheckpointFile writes one record atomically. The content is written to a
// temporary file in the same directory, and a rename replaces the record file.
func writeCheckpointFile(directory string, conversationID string, fingerprint string) (failure error) {
	defer func() {
		if failure != nil {
			slog.Warn("conversation.vectorsearch.checkpoint_write_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"directory", directory,
				"conversation_id", conversationID,
				"err", failure,
			)
			return
		}
		slog.Debug("conversation.vectorsearch.checkpoint_file_written",
			"concern", "conversation.semantic",
			"component", "conversation",
			"directory", directory,
		)
	}()
	if err := os.MkdirAll(directory, checkpointDirMode); err != nil {
		return fmt.Errorf("create checkpoint directory %s: %w", directory, err)
	}
	content, err := json.Marshal(checkpointRecord{ConversationID: conversationID, Fingerprint: fingerprint})
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	sum := sha256.Sum256([]byte(conversationID))
	name := hex.EncodeToString(sum[:])[:checkpointNameLength] + checkpointFileSuffix
	temp, err := os.CreateTemp(directory, checkpointTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create checkpoint temp file: %w", err)
	}
	tempPath := temp.Name()
	if _, writeErr := temp.Write(content); writeErr != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write checkpoint temp file: %w", writeErr)
	}
	if closeErr := temp.Close(); closeErr != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close checkpoint temp file: %w", closeErr)
	}
	if renameErr := os.Rename(tempPath, filepath.Join(directory, name)); renameErr != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace checkpoint file: %w", renameErr)
	}
	return nil
}
