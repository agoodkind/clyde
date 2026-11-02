package vectorsearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// checkpointRecord stores the fingerprint of the latest finished write.
// CommittedWrites lists finished writes that may still have record files.
// One rename saves the fingerprint and finished write IDs together.
type checkpointRecord struct {
	ConversationID  string   `json:"conversation_id"`
	Fingerprint     string   `json:"fingerprint"`
	CommittedWrites []string `json:"committed_writes,omitempty"`
}

// checkpointStore keeps the manifest fingerprint of each conversation after its
// rows are written. A record is one file under <root>/<collection>/, written to
// a temporary file and renamed into place. The store loads a collection's
// records on first use. An empty root keeps the records in memory only.
type checkpointStore struct {
	root   string
	mu     sync.Mutex
	loaded map[string]map[string]string
	// writes stores write records in memory when the store has an empty root.
	writes map[string][]writeRecord
	// memoryClaims lists running writes for a store with an empty root.
	memoryClaims map[string]struct{}
}

func newCheckpointStore(root string) *checkpointStore {
	return &checkpointStore{
		root:         root,
		mu:           sync.Mutex{},
		loaded:       make(map[string]map[string]string),
		writes:       make(map[string][]writeRecord),
		memoryClaims: make(map[string]struct{}),
	}
}

// records returns the fingerprints of a collection and loads its record files
// on first use. The caller must hold the lock. A file that cannot be read or
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

// needed returns sorted manifest conversation IDs when the checkpoint is missing,
// its fingerprint differs, or a write stopped. A forced backfill can stop after a
// checkpoint with the same fingerprint. That conversation still needs ingestion.
func (store *checkpointStore) needed(collectionName string, ids []string, fingerprints []string) ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	recorded := store.records(collectionName)
	stopped, err := store.conversationsWithStoppedWrites(collectionName, ids)
	if err != nil {
		return nil, err
	}
	needed := make([]string, 0)
	for i, id := range ids {
		if id == "" {
			continue
		}
		_, hasStoppedWrite := stopped[id]
		if value, found := recorded[id]; found && value == fingerprints[i] && !hasStoppedWrite {
			continue
		}
		needed = append(needed, id)
	}
	slices.Sort(needed)
	return slices.Compact(needed), nil
}

// record writes the file before updating the memory copy. If the write fails, it
// does not update memory, and ingestion still needs the conversation after a restart.
// The caller must own the lock.
func (store *checkpointStore) record(collectionName string, conversationID string, fingerprint string, committedWrites []string) error {
	recorded := store.records(collectionName)
	if store.root != "" {
		retained := store.committedWritesWithFiles(collectionName, conversationID)
		record := checkpointRecord{
			ConversationID:  conversationID,
			Fingerprint:     fingerprint,
			CommittedWrites: slices.Concat(retained, committedWrites),
		}
		if err := writeCheckpointRecord(filepath.Join(store.root, collectionName), record); err != nil {
			return err
		}
	}
	recorded[conversationID] = fingerprint
	return nil
}

// committedWrites returns the finished write IDs from the checkpoint file.
// If the file is missing or unreadable, it does not return any IDs. Readers then
// treat the write records as unfinished.
func (store *checkpointStore) committedWrites(collectionName string, conversationID string) []string {
	record, _ := store.checkpointFileRecord(collectionName, conversationID)
	return record.CommittedWrites
}

// checkpointFileRecord reads one checkpoint file. It returns false if the file is
// missing or unreadable.
func (store *checkpointStore) checkpointFileRecord(collectionName string, conversationID string) (checkpointRecord, bool) {
	var record checkpointRecord
	path := filepath.Join(store.root, collectionName, checkpointFileName(conversationID))
	content, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("conversation.vectorsearch.checkpoint_read_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"path", path,
				"err", err,
			)
		}
		return record, false
	}
	if err := json.Unmarshal(content, &record); err != nil {
		slog.Warn("conversation.vectorsearch.checkpoint_decode_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"path", path,
			"err", err,
		)
		var empty checkpointRecord
		return empty, false
	}
	return record, true
}

// checkpointFileName returns the first 32 hex characters of the conversation
// ID's SHA-256 hash followed by .json.
func checkpointFileName(conversationID string) string {
	return conversationFileStem(conversationID) + checkpointFileSuffix
}

func conversationFileStem(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return hex.EncodeToString(sum[:])[:checkpointNameLength]
}

// writeCheckpointFile writes a record that does not list any finished write IDs.
func writeCheckpointFile(directory string, conversationID string, fingerprint string) error {
	return writeCheckpointRecord(directory, checkpointRecord{ConversationID: conversationID, Fingerprint: fingerprint, CommittedWrites: nil})
}

// writeCheckpointRecord writes one record atomically. It writes the content to a
// temporary file in the same directory, then renames it to replace the record file.
func writeCheckpointRecord(directory string, record checkpointRecord) (failure error) {
	defer func() {
		if failure != nil {
			slog.Warn("conversation.vectorsearch.checkpoint_write_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"directory", directory,
				"conversation_id", record.ConversationID,
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
	content, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	return writeFileAtomically(directory, checkpointFileName(record.ConversationID), content)
}

// writeFileAtomically writes content to a temporary file, syncs it, renames it to
// name, and syncs the directory.
func writeFileAtomically(directory string, name string, content []byte) error {
	if err := os.MkdirAll(directory, checkpointDirMode); err != nil {
		return failWrite("create checkpoint directory "+directory, err)
	}
	temp, err := os.CreateTemp(directory, checkpointTempPrefix+"*")
	if err != nil {
		return failWrite("create temporary checkpoint file", err)
	}
	tempPath := temp.Name()
	_, writeErr := temp.Write(content)
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	if writeErr != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return failWrite("write temporary checkpoint file", writeErr)
	}
	if closeErr := temp.Close(); closeErr != nil {
		_ = os.Remove(tempPath)
		return failWrite("close temporary checkpoint file", closeErr)
	}
	if renameErr := os.Rename(tempPath, filepath.Join(directory, name)); renameErr != nil {
		_ = os.Remove(tempPath)
		return failWrite("replace checkpoint file", renameErr)
	}
	slog.Debug("conversation.vectorsearch.checkpoint_file_replaced",
		"concern", "conversation.semantic",
		"component", "conversation",
		"directory", directory,
	)
	return syncDirectory(directory)
}

func syncDirectory(directory string) error {
	folder, err := os.Open(directory)
	if err != nil {
		return failWrite("open checkpoint directory", err)
	}
	syncErr := folder.Sync()
	closeErr := folder.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return failWrite("sync checkpoint directory", err)
	}
	return nil
}

// failWrite logs a failed write operation and wraps the error with the operation.
func failWrite(operation string, err error) error {
	slog.Warn("conversation.vectorsearch.checkpoint_file_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}
