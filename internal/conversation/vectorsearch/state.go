package vectorsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/lm-semantic-search/collection"
)

const conversationFilterIDBatchSize = 256

// storedFamilyRow supplies content and split metadata needed to compare a
// message that occupies several stored rows.
type storedFamilyRow struct {
	id                string
	relativePath      string
	content           string
	splitPart         int32
	splitPartRecorded bool
}

func (row storedFamilyRow) hasCurrentKey() bool {
	if !row.splitPartRecorded {
		return false
	}
	return row.id == chunkID(storedChunk{
		Content:              row.content,
		RelativePath:         row.relativePath,
		ConversationID:       "",
		ParentConversationID: "",
		MessageIndex:         0,
		Role:                 "",
		TimestampUnix:        0,
		WorkspaceRoot:        "",
		Archived:             false,
		SplitPart:            row.splitPart,
		LoadRules:            "",
	})
}

type contentComparison int

const (
	contentMatches contentComparison = iota
	contentUnknown
	contentDiffers
)

// conversationState treats all split rows for one message path in one
// conversation as a message family.
type conversationState struct {
	families map[string][]storedFamilyRow
}

func (state *conversationState) hasRow(familyKey string, id string) bool {
	if state == nil {
		return false
	}
	for _, row := range state.families[familyKey] {
		if row.id == id {
			return true
		}
	}
	return false
}

// differs skips families with missing split parts or inconsistent row IDs
// because earlier Clyde revisions wrote those rows.
func (state *conversationState) differs(familyKey string, generated []storedChunk) bool {
	if state == nil {
		return false
	}
	rows := state.families[familyKey]
	for _, row := range rows {
		if !row.hasCurrentKey() {
			return false
		}
	}
	for _, chunk := range generated {
		pathRows := make([]storedFamilyRow, 0, len(rows))
		for _, row := range rows {
			if row.relativePath == chunk.RelativePath {
				pathRows = append(pathRows, row)
			}
		}
		if len(pathRows) == 0 || compareStoredContent(pathRows, chunk.Content, 0) == contentDiffers {
			return true
		}
	}
	return false
}

// compareStoredContent treats a missing piece after offset 0 as unknown because
// the available pieces cannot establish whether the text differs.
func compareStoredContent(rows []storedFamilyRow, content string, offset int) contentComparison {
	if offset == len(content) {
		return contentMatches
	}
	candidates := 0
	unknown := false
	for _, row := range rows {
		whole := offset == 0 && row.splitPart == 0
		if !whole && int(row.splitPart) != offset+1 {
			continue
		}
		candidates++
		if whole {
			if row.content == content {
				return contentMatches
			}
			continue
		}
		if row.content == "" || !strings.HasPrefix(content[offset:], row.content) {
			continue
		}
		switch compareStoredContent(rows, content, offset+len(row.content)) {
		case contentMatches:
			return contentMatches
		case contentUnknown:
			unknown = true
		case contentDiffers:
		}
	}
	if unknown || (candidates == 0 && offset > 0) {
		return contentUnknown
	}
	return contentDiffers
}

// present reports whether the family has a stored row with storable content. A
// row with blank content does not count, because generation writes none.
func (state *conversationState) present(familyKey string) bool {
	if state == nil {
		return false
	}
	_, found := state.families[familyKey]
	return found
}

// contentKey is the reuse key of a row: the hex SHA-256 of its content.
func contentKey(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// embeddingModelsCompatible reports whether a stored vector can serve the
// current model. A row without a recorded model stays reusable.
func embeddingModelsCompatible(stored string, current string) bool {
	return stored == "" || current == "" || stored == current
}

// storedBatch is the stored state of a batch of conversations and the reuse
// vectors of every row read.
type storedBatch struct {
	conversations map[string]*conversationState
	reuse         map[string][]float32
}

func newStoredBatch() *storedBatch {
	return &storedBatch{
		conversations: make(map[string]*conversationState),
		reuse:         make(map[string][]float32),
	}
}

// add records one stored row under its conversation and adds its vector to the
// reuse map when the row's model matches currentModel.
func (batch *storedBatch) add(conversationID string, row collection.StoredRow, currentModel string) {
	if len(row.Vector) > 0 && embeddingModelsCompatible(row.EmbeddingModel, currentModel) {
		batch.reuse[contentKey(row.Content)] = row.Vector
	}
	if conversationID == "" || !conversationTextIsStorable(row.Content) {
		return
	}
	state, found := batch.conversations[conversationID]
	if !found {
		state = &conversationState{families: make(map[string][]storedFamilyRow)}
		batch.conversations[conversationID] = state
	}
	familyKey := chunkFamilyKey(conversationID, row.RelativePath)
	state.families[familyKey] = append(state.families[familyKey], storedFamilyRow{
		id:                row.ID,
		relativePath:      row.RelativePath,
		content:           row.Content,
		splitPart:         row.SplitPart,
		splitPartRecorded: row.SplitPartRecorded,
	})
}

func (batch *storedBatch) state(conversationID string) *conversationState {
	return batch.conversations[conversationID]
}

// assignConversationID returns the requested conversation a stored row belongs
// to. A row with a conversationId cell in the request belongs to that
// conversation. A row without one belongs to the requested conversation with the
// longest matching family path prefix, which covers rows written before the
// column existed.
func assignConversationID(row collection.StoredRow, requested []string) string {
	if cell, found := row.Scalars[conversationIDColumn]; found && cell.State == collection.ScalarCellValue {
		if cell.Value.String != "" && slices.Contains(requested, cell.Value.String) {
			return cell.Value.String
		}
	}
	matchedID := ""
	matchedLength := 0
	for _, requestedID := range requested {
		prefixes := []string{
			conversationRelativePathPrefix(requestedID),
			conversationToolRelativePathPrefix(requestedID),
			conversationThinkingRelativePathPrefix(requestedID),
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(row.RelativePath, prefix) && len(prefix) > matchedLength {
				matchedID = requestedID
				matchedLength = len(prefix)
			}
		}
	}
	return matchedID
}

// storedRowsDeclaration is the declaration of a stored-row request: the
// conversation ID column that selects rows and reads back.
func storedRowsDeclaration() collection.Declaration {
	return collection.Declaration{
		ItemIDColumn: conversationIDColumn,
		Scalars:      []collection.ScalarColumn{nullableStringColumn(conversationIDColumn, conversationIDMaxLength)},
	}
}

// loadStoredBatch reads every stored row of the requested conversations. It
// selects a row by its conversationId value or by a conversation path prefix;
// the prefix matches rows written before the column existed. A missing
// collection returns an empty batch.
func (c *Client) loadStoredBatch(ctx context.Context, collectionName string, conversationIDs []string) (*storedBatch, error) {
	batch := newStoredBatch()
	requested := dedupeIDs(conversationIDs)
	if len(requested) == 0 {
		return batch, nil
	}
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return batch, nil
	}
	for start := 0; start < len(requested); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(requested))
		group := requested[start:end]
		prefixes := make([]string, 0, len(group)*3)
		for _, id := range group {
			prefixes = append(prefixes,
				conversationRelativePathPrefix(id),
				conversationToolRelativePathPrefix(id),
				conversationThinkingRelativePathPrefix(id),
			)
		}
		rows, queryErr := c.store.QueryRows(ctx, collection.RowsRequest{
			Collection:    collectionName,
			Declaration:   storedRowsDeclaration(),
			ItemIDs:       group,
			PathPrefixes:  prefixes,
			IncludeVector: true,
		})
		if queryErr != nil {
			return nil, failRead("load stored conversation rows from "+collectionName, queryErr)
		}
		for _, row := range rows {
			batch.add(assignConversationID(row, group), row, c.embeddingModel)
		}
	}
	return batch, nil
}

// failRead logs one failed request and returns the error with the operation
// that failed.
func failRead(operation string, err error) error {
	slog.Warn("conversation.vectorsearch.request_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, found := seen[trimmed]; found {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}
