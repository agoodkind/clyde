package vectorsearch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	// writeProgressDirName identifies a folder beside the checkpoint collection folders.
	writeProgressDirName = ".in-progress"
	// writeRecordVersion selects the write record format. The daemon rejects records
	// with another version.
	writeRecordVersion = 1
	// familyProofEncoding prefixes every family proof's encoded fields. Change this
	// value when the encoded fields change.
	familyProofEncoding  = "clyde-family-proof-v1"
	writeIDBytes         = 16
	claimFileSuffix      = ".lock"
	writeRecordSeparator = "."
	// claimAttempts limits retries when another client removes a claim file between
	// opening and locking it.
	claimAttempts = 3
)

// ErrConversationWriteBusy reports that another client or call is writing the
// conversation. The worker treats "conflicting active job" as a temporary condition.
var ErrConversationWriteBusy = errors.New("conflicting active job writes this conversation")

// familyProof pairs a family key with the hash of its projected rows.
type familyProof struct {
	Family string `json:"family"`
	Proof  string `json:"proof"`
}

// writeRecord records one attempt to write a conversation. Its file exists before
// the first row write and until the checkpoint lists its write ID. The write ID
// distinguishes attempts with the same manifest fingerprint.
//
// Retained marks stopped writes for families that later changed. The record supports
// recovery if the earlier projection returns. It does not mark the conversation for
// ingestion.
type writeRecord struct {
	Version        int           `json:"version"`
	WriteID        string        `json:"write_id"`
	ConversationID string        `json:"conversation_id"`
	Fingerprint    string        `json:"fingerprint"`
	Retained       bool          `json:"retained,omitempty"`
	Families       []familyProof `json:"families"`
}

// validate checks each field that a reader uses to identify the stopped write. An
// empty fingerprint is valid for a conversation outside the manifest.
func (record writeRecord) validate(path string) error {
	if record.Version != writeRecordVersion {
		return fmt.Errorf("write record %s has version %d; expected %d", path, record.Version, writeRecordVersion)
	}
	if record.WriteID == "" || record.ConversationID == "" || len(record.Families) == 0 {
		return fmt.Errorf("write record %s lacks a write ID, conversation ID, or family", path)
	}
	for _, family := range record.Families {
		if family.Family == "" || family.Proof == "" {
			return fmt.Errorf("write record %s has a family without a key or proof", path)
		}
	}
	want := conversationFileStem(record.ConversationID) + writeRecordSeparator + record.WriteID + checkpointFileSuffix
	if filepath.Base(path) != want {
		return fmt.Errorf("write record %s does not match its conversation and write IDs", path)
	}
	return nil
}

// storedWrite pairs a write record with its file path. The path is empty when the
// store has an empty root.
type storedWrite struct {
	record writeRecord
	path   string
}

// conversationClaim grants exclusive write access to one conversation. A call claims
// it before reading stopped writes and releases it after the write attempt ends.
// A store with a root uses a kernel lock that the kernel releases on process exit.
// A store with an empty root uses a memory entry. A write record without a claim
// represents a stopped write.
type conversationClaim struct {
	file   *os.File
	path   string
	store  *checkpointStore
	memory string
}

// release removes the claim file before releasing its lock. A client that opened
// the old file detects the removal and opens a new file.
func (claim *conversationClaim) release() {
	if claim == nil {
		return
	}
	if claim.memory != "" {
		claim.store.mu.Lock()
		defer claim.store.mu.Unlock()
		delete(claim.store.memoryClaims, claim.memory)
		claim.memory = ""
		return
	}
	if claim.file == nil {
		return
	}
	removeErr := os.Remove(claim.path)
	if os.IsNotExist(removeErr) {
		removeErr = nil
	}
	if err := errors.Join(removeErr, claim.file.Close()); err != nil {
		slog.Warn("conversation.vectorsearch.claim_release_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"path", claim.path,
			"err", err,
		)
	}
	claim.file = nil
}

func (store *checkpointStore) writeRecordDirectory(collectionName string) string {
	return filepath.Join(store.root, writeProgressDirName, collectionName)
}

func (store *checkpointStore) claimPath(collectionName string, conversationID string) string {
	return filepath.Join(store.writeRecordDirectory(collectionName), conversationFileStem(conversationID)+claimFileSuffix)
}

func (store *checkpointStore) writeRecordPath(collectionName string, conversationID string, writeID string) string {
	name := conversationFileStem(conversationID) + writeRecordSeparator + writeID + checkpointFileSuffix
	return filepath.Join(store.writeRecordDirectory(collectionName), name)
}

func memoryWritesKey(collectionName string, conversationID string) string {
	return collectionName + "\x00" + conversationID
}

// claimConversation claims exclusive write access to a conversation. It returns
// ErrConversationWriteBusy when another client or call owns the claim.
func (store *checkpointStore) claimConversation(collectionName string, conversationID string) (*conversationClaim, error) {
	if store.root == "" {
		key := memoryWritesKey(collectionName, conversationID)
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, claimed := store.memoryClaims[key]; claimed {
			return nil, ErrConversationWriteBusy
		}
		store.memoryClaims[key] = struct{}{}
		return &conversationClaim{file: nil, path: "", store: store, memory: key}, nil
	}
	if err := os.MkdirAll(store.writeRecordDirectory(collectionName), checkpointDirMode); err != nil {
		return nil, failWrite("create write record directory", err)
	}
	path := store.claimPath(collectionName, conversationID)
	for range claimAttempts {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, failWrite("open conversation claim", err)
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, ErrConversationWriteBusy
		}
		if err != nil {
			_ = file.Close()
			return nil, failWrite("lock conversation claim", err)
		}
		if claimFileCurrent(file, path) {
			return &conversationClaim{file: file, path: path, store: store, memory: ""}, nil
		}
		_ = file.Close()
	}
	return nil, ErrConversationWriteBusy
}

func claimFileCurrent(file *os.File, path string) bool {
	opened, err := file.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(opened, current)
}

// conversationClaimed checks whether another client owns the claim. Shared probes
// do not conflict with one another. If opening the claim file fails, it returns
// false. Reading the record reports directory errors.
func (store *checkpointStore) conversationClaimed(collectionName string, conversationID string) bool {
	file, err := os.Open(store.claimPath(collectionName, conversationID))
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	return errors.Is(syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

func readWriteRecord(path string) (writeRecord, error) {
	var record writeRecord
	content, err := os.ReadFile(path)
	if err != nil {
		return record, failRead("read write record", err)
	}
	if err := json.Unmarshal(content, &record); err != nil {
		return record, failRead("decode write record "+path, err)
	}
	return record, record.validate(path)
}

func isWriteRecordName(name string) bool {
	return strings.HasSuffix(name, checkpointFileSuffix) && !strings.HasPrefix(name, checkpointTempPrefix)
}

// stoppedWrites returns conversation write attempts that stopped before recording a
// checkpoint. The caller must own the conversation claim. If reading a record fails,
// the call returns an error rather than an incomplete list of stopped writes.
func (store *checkpointStore) stoppedWrites(collectionName string, conversationID string) ([]storedWrite, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.root == "" {
		records := store.writes[memoryWritesKey(collectionName, conversationID)]
		stopped := make([]storedWrite, 0, len(records))
		for _, record := range records {
			stopped = append(stopped, storedWrite{record: record, path: ""})
		}
		return stopped, nil
	}
	directory := store.writeRecordDirectory(collectionName)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, failRead("read write record directory", err)
	}
	prefix := conversationFileStem(conversationID) + writeRecordSeparator
	committed := store.committedWrites(collectionName, conversationID)
	stopped := make([]storedWrite, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !isWriteRecordName(entry.Name()) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, readErr := readWriteRecord(path)
		if readErr != nil {
			return nil, readErr
		}
		if !slices.Contains(committed, record.WriteID) {
			stopped = append(stopped, storedWrite{record: record, path: path})
		}
	}
	return stopped, nil
}

// conversationsWithStoppedWrites returns conversations with stopped writes, excluding
// retained records and writes that another client is running. It retries removal of
// finished write records. Directory errors fail the call rather than allowing a
// matching checkpoint to conceal unfinished work. If reading a record fails, the
// conversation still needs ingestion, and its next upsert reports the error.
// The caller must own the store lock.
func (store *checkpointStore) conversationsWithStoppedWrites(collectionName string, ids []string) (map[string]struct{}, error) {
	conversations := make(map[string]struct{})
	if store.root == "" {
		for _, id := range ids {
			key := memoryWritesKey(collectionName, id)
			if _, claimed := store.memoryClaims[key]; claimed {
				continue
			}
			for _, record := range store.writes[key] {
				if !record.Retained {
					conversations[id] = struct{}{}
				}
			}
		}
		return conversations, nil
	}
	directory := store.writeRecordDirectory(collectionName)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return conversations, nil
	}
	if err != nil {
		return nil, failRead("read write record directory", err)
	}
	idsByStem := make(map[string]string, len(ids))
	for _, id := range ids {
		idsByStem[conversationFileStem(id)] = id
	}
	finished := make(map[string]struct{})
	for _, entry := range entries {
		if !isWriteRecordName(entry.Name()) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, readErr := readWriteRecord(path)
		if readErr != nil {
			logWriteRecordFailure(path, readErr)
			stem, _, _ := strings.Cut(entry.Name(), writeRecordSeparator)
			if id, inManifest := idsByStem[stem]; inManifest {
				conversations[id] = struct{}{}
			}
			continue
		}
		if slices.Contains(store.committedWrites(collectionName, record.ConversationID), record.WriteID) {
			finished[record.ConversationID] = struct{}{}
			continue
		}
		if !record.Retained && !store.conversationClaimed(collectionName, record.ConversationID) {
			conversations[record.ConversationID] = struct{}{}
		}
	}
	for conversationID := range finished {
		store.retryFinishedCleanup(collectionName, conversationID)
	}
	return conversations, nil
}

// retryFinishedCleanup claims the conversation before removing finished write records.
func (store *checkpointStore) retryFinishedCleanup(collectionName string, conversationID string) {
	claim, err := store.claimConversation(collectionName, conversationID)
	if err != nil {
		if !errors.Is(err, ErrConversationWriteBusy) {
			logWriteRecordFailure(store.claimPath(collectionName, conversationID), err)
		}
		return
	}
	defer claim.release()
	store.removeFinishedRecords(collectionName, conversationID)
}

// beginWrite stores a record for this attempt. The caller must own the conversation
// claim and write rows only after this call succeeds.
func (store *checkpointStore) beginWrite(collectionName string, conversationID string, fingerprint string, families []familyProof) (storedWrite, error) {
	identity := make([]byte, writeIDBytes)
	if _, err := rand.Read(identity); err != nil {
		return storedWrite{}, failWrite("create write ID", err)
	}
	record := writeRecord{
		Version:        writeRecordVersion,
		WriteID:        hex.EncodeToString(identity),
		ConversationID: conversationID,
		Fingerprint:    fingerprint,
		Retained:       false,
		Families:       families,
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.root == "" {
		key := memoryWritesKey(collectionName, conversationID)
		store.writes[key] = append(store.writes[key], record)
		return storedWrite{record: record, path: ""}, nil
	}
	content, err := json.Marshal(record)
	if err != nil {
		return storedWrite{}, failWrite("encode write record", err)
	}
	path := store.writeRecordPath(collectionName, conversationID, record.WriteID)
	if err := writeFileAtomically(filepath.Dir(path), filepath.Base(path), content); err != nil {
		return storedWrite{}, err
	}
	return storedWrite{record: record, path: path}, nil
}

// commitWrite records a checkpoint with this attempt and the stopped attempts it
// resolved. One rename saves those write IDs and the fingerprint together. Readers
// treat listed IDs as finished even if later record updates fail. An unlisted,
// unretained record still marks its conversation for ingestion.
func (store *checkpointStore) commitWrite(collectionName string, conversationID string, fingerprint string, active *storedWrite, stopped []storedWrite, proofs map[string]string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	committed := make([]string, 0, len(stopped)+1)
	if active != nil {
		committed = append(committed, active.record.WriteID)
	}
	retained := make([]storedWrite, 0)
	for _, write := range stopped {
		changed := changedFamilies(write.record.Families, proofs)
		if len(changed) > 0 {
			write.record.Retained = true
			write.record.Families = changed
			retained = append(retained, write)
			continue
		}
		committed = append(committed, write.record.WriteID)
	}
	if err := store.record(collectionName, conversationID, fingerprint, committed); err != nil {
		return err
	}
	if store.root == "" {
		kept := make([]writeRecord, 0, len(retained))
		for _, write := range retained {
			kept = append(kept, write.record)
		}
		store.writes[memoryWritesKey(collectionName, conversationID)] = kept
		return nil
	}
	for _, write := range retained {
		content, err := json.Marshal(write.record)
		if err == nil {
			err = writeFileAtomically(filepath.Dir(write.path), filepath.Base(write.path), content)
		}
		if err != nil {
			logWriteRecordFailure(write.path, err)
		}
	}
	store.removeFinishedRecords(collectionName, conversationID)
	return nil
}

// supersedeStoppedWrites replaces earlier records after a partial write. The active
// record lists families with unwritten rows. The function removes earlier records
// for the same projection and retains records for changed families. If an update
// fails, the older record remains until a later checkpoint resolves it.
func (store *checkpointStore) supersedeStoppedWrites(collectionName string, conversationID string, active *storedWrite, stopped []storedWrite, proofs map[string]string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	retained := make([]writeRecord, 0, len(stopped)+1)
	for _, write := range stopped {
		changed := changedFamilies(write.record.Families, proofs)
		if len(changed) == 0 {
			if store.root != "" {
				if err := os.Remove(write.path); err != nil && !os.IsNotExist(err) {
					logWriteRecordFailure(write.path, err)
				}
			}
			continue
		}
		write.record.Retained = true
		write.record.Families = changed
		retained = append(retained, write.record)
		if store.root == "" {
			continue
		}
		content, err := json.Marshal(write.record)
		if err == nil {
			err = writeFileAtomically(filepath.Dir(write.path), filepath.Base(write.path), content)
		}
		if err != nil {
			logWriteRecordFailure(write.path, err)
		}
	}
	if store.root == "" {
		if active != nil {
			retained = append(retained, active.record)
		}
		store.writes[memoryWritesKey(collectionName, conversationID)] = retained
	}
	slog.Debug("conversation.vectorsearch.stopped_writes_superseded",
		"concern", "conversation.semantic",
		"component", "conversation",
		"conversation_id", conversationID,
		"retained", len(retained),
	)
}

// removeFinishedRecords removes records for write IDs in the checkpoint, then rewrites
// the checkpoint with IDs for records that remain. A later manifest sync retries
// failed removals. The caller must own the conversation claim and store lock.
func (store *checkpointStore) removeFinishedRecords(collectionName string, conversationID string) {
	record, found := store.checkpointFileRecord(collectionName, conversationID)
	if !found || len(record.CommittedWrites) == 0 {
		return
	}
	remaining := make([]string, 0, len(record.CommittedWrites))
	for _, writeID := range record.CommittedWrites {
		path := store.writeRecordPath(collectionName, conversationID, writeID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logWriteRecordFailure(path, err)
			remaining = append(remaining, writeID)
		}
	}
	if len(remaining) == len(record.CommittedWrites) {
		return
	}
	record.CommittedWrites = remaining
	if err := writeCheckpointRecord(filepath.Join(store.root, collectionName), record); err != nil {
		logWriteRecordFailure(collectionName, err)
	}
	slog.Debug("conversation.vectorsearch.finished_records_removed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"conversation_id", conversationID,
		"remaining", len(remaining),
	)
}

func logWriteRecordFailure(path string, err error) {
	slog.Warn("conversation.vectorsearch.write_record_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"path", path,
		"err", err,
	)
}

// committedWritesWithFiles returns finished write IDs unless a file lookup confirms
// that their records are missing. Other lookup errors preserve the IDs.
func (store *checkpointStore) committedWritesWithFiles(collectionName string, conversationID string) []string {
	committed := store.committedWrites(collectionName, conversationID)
	retained := make([]string, 0, len(committed))
	for _, writeID := range committed {
		path := store.writeRecordPath(collectionName, conversationID, writeID)
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			logWriteRecordFailure(path, err)
		}
		retained = append(retained, writeID)
	}
	return retained
}

func changedFamilies(families []familyProof, proofs map[string]string) []familyProof {
	changed := make([]familyProof, 0, len(families))
	for _, family := range families {
		if proofs[family.Family] != family.Proof {
			changed = append(changed, family)
		}
	}
	return changed
}

// stoppedFamilies returns the families with a stopped write of their current projection.
func stoppedFamilies(stopped []storedWrite, proofs map[string]string) map[string]struct{} {
	families := make(map[string]struct{})
	for _, write := range stopped {
		for _, family := range write.record.Families {
			if proofs[family.Family] == family.Proof {
				families[family.Family] = struct{}{}
			}
		}
	}
	return families
}

// markedFamilies returns families with stopped writes for any projection. Those
// records establish that stored families may be incomplete. The plan writes pieces
// of the current projection without treating a stored prefix as a complete family.
func markedFamilies(stopped []storedWrite) map[string]struct{} {
	families := make(map[string]struct{})
	for _, write := range stopped {
		for _, family := range write.record.Families {
			families[family.Family] = struct{}{}
		}
	}
	return families
}

// plannedFamilyProofs returns one entry for each family with planned chunks.
func plannedFamilyProofs(conversationID string, chunks []storedChunk, proofs map[string]string) []familyProof {
	families := groupFamilies(conversationID, chunks)
	planned := make([]familyProof, 0, len(families))
	for _, family := range families {
		planned = append(planned, familyProof{Family: family.Key, Proof: proofs[family.Key]})
	}
	return planned
}

// writeProofField writes the field name, value length, and value to delimit each field.
func writeProofField(digest hash.Hash, name string, value string) {
	_, _ = digest.Write([]byte(name + "=" + strconv.Itoa(len(value)) + ":" + value + "\n"))
}

// familyProofs hashes the embedding model and each family's projected row fields.
// The model identifies which vectors the client can reuse. The encoding includes
// explicit fields rather than the Go type's layout. Changing that type does not
// change stored proofs.
func familyProofs(families []chunkFamily, embeddingModel string) map[string]string {
	proofs := make(map[string]string, len(families))
	for _, family := range families {
		digest := sha256.New()
		writeProofField(digest, "encoding", familyProofEncoding)
		writeProofField(digest, "embedding_model", embeddingModel)
		writeProofField(digest, "family", family.Key)
		for _, chunk := range family.Chunks {
			writeProofField(digest, "relative_path", chunk.RelativePath)
			writeProofField(digest, "conversation_id", chunk.ConversationID)
			writeProofField(digest, "parent_conversation_id", chunk.ParentConversationID)
			writeProofField(digest, "message_index", strconv.FormatInt(int64(chunk.MessageIndex), 10))
			writeProofField(digest, "role", chunk.Role)
			writeProofField(digest, "timestamp_unix", strconv.FormatInt(chunk.TimestampUnix, 10))
			writeProofField(digest, "workspace_root", chunk.WorkspaceRoot)
			writeProofField(digest, "archived", strconv.FormatBool(chunk.Archived))
			writeProofField(digest, "split_part", strconv.FormatInt(int64(chunk.SplitPart), 10))
			writeProofField(digest, "load_rules", chunk.LoadRules)
			writeProofField(digest, "content", chunk.Content)
		}
		proofs[family.Key] = hex.EncodeToString(digest.Sum(nil))
	}
	return proofs
}
