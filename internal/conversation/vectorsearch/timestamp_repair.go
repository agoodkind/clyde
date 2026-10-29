package vectorsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	metadataTimestampKey          = "timestamp_unix"
	timestampRepairWriteBatchRows = 500
)

// A parser uses the zero time for a message without a timestamp.
var missingTimestampUnix = time.Time{}.Unix()

// ErrTimestampRepairUnsupported reports that timestamp repair requires the
// single-collection Milvus layout. The shared layout also stores timestamp
// ranges on vector rows.
var ErrTimestampRepairUnsupported = errors.New("repair timestamps: use the single-collection Milvus layout")

// TimestampRepairSource supplies the current documents for one conversation.
type TimestampRepairSource struct {
	ConversationID string
	Documents      []semsearch.SemDoc
}

// TimestampRepairReport counts conversations and classifies each stored row
// that the repair reads. RowsWritten counts the changed rows that Milvus wrote.
type TimestampRepairReport struct {
	Conversations int `json:"conversations"`
	RowsRead      int `json:"rows_read"`
	// RowsCurrent counts matched rows with the source timestamp in both fields.
	RowsCurrent int `json:"rows_current"`
	// RowsChanged counts matched rows that need a timestamp update.
	RowsChanged int `json:"rows_changed"`
	// RowsWritten counts changed rows that Milvus updated.
	RowsWritten int `json:"rows_written"`
	// RowsUnmatched counts rows without a source match.
	RowsUnmatched int `json:"rows_unmatched"`
	// RowsMissingTimestamp counts matched rows when the source has a zero time.
	RowsMissingTimestamp int `json:"rows_missing_timestamp"`
	// RowsAmbiguous counts rows with matching source pieces that have different timestamps.
	RowsAmbiguous int `json:"rows_ambiguous"`
	// RowsInvalidMetadata counts matched rows with invalid timestamp metadata.
	RowsInvalidMetadata int `json:"rows_invalid_metadata"`
}

// Add combines another report's counts with this report.
func (report *TimestampRepairReport) Add(other TimestampRepairReport) {
	report.Conversations += other.Conversations
	report.RowsRead += other.RowsRead
	report.RowsCurrent += other.RowsCurrent
	report.RowsChanged += other.RowsChanged
	report.RowsWritten += other.RowsWritten
	report.RowsUnmatched += other.RowsUnmatched
	report.RowsMissingTimestamp += other.RowsMissingTimestamp
	report.RowsAmbiguous += other.RowsAmbiguous
	report.RowsInvalidMetadata += other.RowsInvalidMetadata
}

type sourcePiece struct {
	content       string
	timestampUnix int64
}

type timestampUpdate struct {
	id            string
	relativePath  string
	metadata      string
	timestampUnix int64
}

type matchedRow struct {
	relativePath  string
	timestampUnix int64
}

type timestampMatch int

const (
	timestampMatchFound timestampMatch = iota
	timestampMatchNone
	timestampMatchMissing
	timestampMatchAmbiguous
)

// RepairConversationTimestamps compares stored rows with source documents and
// updates their timestamps. A row matches when its relativePath matches a source
// row and its content equals the source bytes at the stored split position.
// Milvus updates only timestampUnix and the timestamp_unix metadata value. The
// repair skips unmatched rows, source messages without timestamps, ambiguous
// matches, and invalid metadata. A dry run counts rows without writing them.
// A missing collection returns ErrCollectionAbsent.
func (c *Client) RepairConversationTimestamps(
	ctx context.Context,
	collectionID string,
	sources []TimestampRepairSource,
	dryRun bool,
) (TimestampRepairReport, error) {
	report := TimestampRepairReport{
		Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0,
		RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
	}
	if c == nil {
		return report, errors.New("repair conversation timestamps: provide a client")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus || milvus.shared != nil {
		return report, ErrTimestampRepairUnsupported
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return report, errors.New("repair conversation timestamps: provide a collection ID")
	}
	pieces, order, err := sourcePieces(sources)
	if err != nil {
		return report, err
	}
	report.Conversations = len(order)
	if len(order) == 0 {
		return report, nil
	}
	collectionName := CollectionName(trimmedCollectionID)
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return report, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.timestamp_repair_collection_absent",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", collectionName,
		)
		return report, fmt.Errorf("repair timestamps in collection %s: %w", collectionName, ErrCollectionAbsent)
	}
	matched, err := c.matchStoredTimestamps(ctx, collectionName, order, pieces, &report)
	if err != nil {
		return report, err
	}
	updates, err := plannedTimestampUpdates(ctx, milvus, collectionName, matched, &report)
	if err != nil {
		return report, err
	}
	if !dryRun {
		columns := declaredColumns(c.declaration, timestampUnixColumn)
		for start := 0; start < len(updates); start += timestampRepairWriteBatchRows {
			end := min(start+timestampRepairWriteBatchRows, len(updates))
			if err := milvus.writeRowTimestamps(ctx, collectionName, columns, updates[start:end]); err != nil {
				return report, err
			}
			report.RowsWritten += end - start
		}
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.timestamp_repair_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"conversations", report.Conversations,
		"rows_read", report.RowsRead,
		"rows_current", report.RowsCurrent,
		"rows_changed", report.RowsChanged,
		"rows_written", report.RowsWritten,
		"rows_unmatched", report.RowsUnmatched,
		"rows_missing_timestamp", report.RowsMissingTimestamp,
		"rows_ambiguous", report.RowsAmbiguous,
		"rows_invalid_metadata", report.RowsInvalidMetadata,
		"dry_run", dryRun,
	)
	return report, nil
}

func sourcePieces(sources []TimestampRepairSource) (map[string]map[string][]sourcePiece, []string, error) {
	pieces := make(map[string]map[string][]sourcePiece, len(sources))
	order := make([]string, 0, len(sources))
	for _, source := range sources {
		conversationID := strings.TrimSpace(source.ConversationID)
		if conversationID == "" {
			return nil, nil, errors.New("repair conversation timestamps: provide a conversation ID")
		}
		if _, duplicate := pieces[conversationID]; duplicate {
			return nil, nil, fmt.Errorf("repair conversation timestamps: remove the duplicate conversation %s", conversationID)
		}
		byPath := make(map[string][]sourcePiece)
		for _, doc := range source.Documents {
			if strings.TrimSpace(doc.ConversationID) != conversationID {
				return nil, nil, fmt.Errorf("repair conversation timestamps: document for %q does not match conversation %s", doc.ConversationID, conversationID)
			}
			chunks, err := documentChunks(doc, conversationChunkMaxBytes)
			if err != nil {
				slog.Warn("conversation.vectorsearch.timestamp_repair_rows_failed",
					"concern", "conversation.semantic",
					"component", "conversation",
					"conversation_id", conversationID,
					"err", err,
				)
				return nil, nil, fmt.Errorf("repair conversation timestamps: build rows for %s: %w", conversationID, err)
			}
			for _, chunk := range chunks {
				byPath[chunk.RelativePath] = append(byPath[chunk.RelativePath], sourcePiece{content: chunk.Content, timestampUnix: chunk.TimestampUnix})
			}
		}
		pieces[conversationID] = byPath
		order = append(order, conversationID)
	}
	return pieces, order, nil
}

func (c *Client) matchStoredTimestamps(
	ctx context.Context,
	collectionName string,
	order []string,
	pieces map[string]map[string][]sourcePiece,
	report *TimestampRepairReport,
) (map[string]matchedRow, error) {
	matched := make(map[string]matchedRow)
	for start := 0; start < len(order); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(order))
		group := order[start:end]
		prefixes := make([]string, 0, len(group)*3)
		for _, conversationID := range group {
			prefixes = append(prefixes,
				conversationRelativePathPrefix(conversationID),
				conversationToolRelativePathPrefix(conversationID),
				conversationThinkingRelativePathPrefix(conversationID),
			)
		}
		rows, err := c.store.QueryRows(ctx, collection.RowsRequest{
			Collection:    collectionName,
			Declaration:   storedRowsDeclaration(),
			ItemIDs:       group,
			PathPrefixes:  prefixes,
			IncludeVector: false,
		})
		if err != nil {
			return nil, failRead("read conversation rows from collection "+collectionName, err)
		}
		for _, row := range rows {
			report.RowsRead++
			conversationID := assignConversationID(row, group)
			timestampUnix, outcome := matchRowTimestamp(row, pieces[conversationID][row.RelativePath])
			switch outcome {
			case timestampMatchFound:
				matched[row.ID] = matchedRow{relativePath: row.RelativePath, timestampUnix: timestampUnix}
			case timestampMatchNone:
				report.RowsUnmatched++
			case timestampMatchMissing:
				report.RowsMissingTimestamp++
			case timestampMatchAmbiguous:
				report.RowsAmbiguous++
			}
		}
	}
	return matched, nil
}

func matchRowTimestamp(row collection.StoredRow, candidates []sourcePiece) (int64, timestampMatch) {
	found := false
	var timestampUnix int64
	for _, candidate := range candidates {
		if !pieceHoldsRow(candidate.content, row) {
			continue
		}
		if found && candidate.timestampUnix != timestampUnix {
			return 0, timestampMatchAmbiguous
		}
		found = true
		timestampUnix = candidate.timestampUnix
	}
	if !found {
		return 0, timestampMatchNone
	}
	if timestampUnix == missingTimestampUnix {
		return 0, timestampMatchMissing
	}
	return timestampUnix, timestampMatchFound
}

// A positive split position is one more than the byte offset in the source.
// An absent or zero split position requires a match against the whole piece.
func pieceHoldsRow(piece string, row collection.StoredRow) bool {
	if !row.SplitPartRecorded || row.SplitPart == 0 {
		return piece == row.Content
	}
	offset := int(row.SplitPart) - 1
	if offset < 0 || row.Content == "" || offset+len(row.Content) > len(piece) {
		return false
	}
	return piece[offset:offset+len(row.Content)] == row.Content
}

func plannedTimestampUpdates(
	ctx context.Context,
	milvus *milvusBackend,
	collectionName string,
	matched map[string]matchedRow,
	report *TimestampRepairReport,
) ([]timestampUpdate, error) {
	ids := make([]string, 0, len(matched))
	for id := range matched {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	stored, err := milvus.readRowTimestamps(ctx, collectionName, ids)
	if err != nil {
		return nil, err
	}
	updates := make([]timestampUpdate, 0)
	for _, id := range ids {
		row, found := stored[id]
		if !found {
			// Another operation deleted the row after the first read.
			report.RowsUnmatched++
			continue
		}
		source := matched[id]
		want := source.timestampUnix
		rewritten, metadataTimestamp, err := replaceMetadataTimestamp(id, row.metadata, want)
		if err != nil {
			report.RowsInvalidMetadata++
			continue
		}
		if metadataTimestamp == want && row.timestampKnown && row.timestampUnix == want {
			report.RowsCurrent++
			continue
		}
		report.RowsChanged++
		updates = append(updates, timestampUpdate{id: id, relativePath: source.relativePath, metadata: rewritten, timestampUnix: want})
	}
	return updates, nil
}

// Replace only the value bytes to preserve all other metadata bytes.
func replaceMetadataTimestamp(rowID string, metadata string, timestampUnix int64) (rewritten string, stored int64, err error) {
	defer func() {
		if err != nil {
			slog.Warn("conversation.vectorsearch.timestamp_repair_metadata_invalid",
				"concern", "conversation.semantic",
				"component", "conversation",
				"row_id", rowID,
				"err", err,
			)
		}
	}()
	decoder := json.NewDecoder(strings.NewReader(metadata))
	opening, err := decoder.Token()
	if err != nil {
		return "", 0, fmt.Errorf("read the metadata object: %w", err)
	}
	if delimiter, isDelimiter := opening.(json.Delim); !isDelimiter || delimiter != '{' {
		return "", 0, errors.New("metadata must be a JSON object")
	}
	found := false
	valueStart := 0
	valueEnd := 0
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return "", 0, fmt.Errorf("read a metadata key: %w", keyErr)
		}
		key, isString := keyToken.(string)
		if !isString {
			return "", 0, errors.New("metadata keys must be strings")
		}
		var raw json.RawMessage
		if decodeErr := decoder.Decode(&raw); decodeErr != nil {
			return "", 0, fmt.Errorf("read metadata value for %s: %w", key, decodeErr)
		}
		if key != metadataTimestampKey {
			continue
		}
		if found {
			return "", 0, errors.New("metadata must contain timestamp_unix once")
		}
		found = true
		valueEnd = int(decoder.InputOffset())
		valueStart = valueEnd - len(raw)
		if valueStart < 0 || metadata[valueStart:valueEnd] != string(raw) {
			return "", 0, errors.New("cannot locate timestamp_unix in metadata")
		}
		parsed, parseErr := strconv.ParseInt(string(raw), 10, 64)
		if parseErr != nil {
			return "", 0, fmt.Errorf("parse metadata timestamp_unix as an integer: %w", parseErr)
		}
		stored = parsed
	}
	if _, err := decoder.Token(); err != nil {
		return "", 0, fmt.Errorf("read the end of the metadata object: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", 0, errors.New("metadata must not contain data after the JSON object")
	}
	if !found {
		return "", 0, errors.New("metadata must contain timestamp_unix")
	}
	return metadata[:valueStart] + strconv.FormatInt(timestampUnix, 10) + metadata[valueEnd:], stored, nil
}
