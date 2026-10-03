package vectorsearch

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// storedRow is one row read from the live conversation collection.
type storedRow struct {
	ID                string
	RelativePath      string
	Content           string
	Role              string
	EmbeddingModel    string
	SplitPart         int32
	SplitPartRecorded bool
	Vector            []float32
}

// conversationState is the stored rows of one conversation grouped by family
// key.
type conversationState struct {
	families map[string][]storedRow
}

func newConversationState() *conversationState {
	return &conversationState{families: make(map[string][]storedRow)}
}

// present reports whether the family has a stored row with storable content. A
// row with blank content does not count, because generation writes none.
func (state *conversationState) present(familyKey string) bool {
	if state == nil {
		return false
	}
	for _, row := range state.families[familyKey] {
		if conversationTextIsStorable(row.Content) {
			return true
		}
	}
	return false
}

// rows returns the stored rows of one family.
func (state *conversationState) rows(familyKey string) []storedRow {
	if state == nil {
		return nil
	}
	return state.families[familyKey]
}

// storedTextOf rebuilds the text of a message family by concatenating its rows
// in part order. It also returns the role of the first row that has one.
func storedTextOf(conversationID string, rows []storedRow) (string, string) {
	parts := slices.Clone(rows)
	slices.SortStableFunc(parts, func(left storedRow, right storedRow) int {
		return compareStoredParts(conversationID, left, right)
	})
	var text strings.Builder
	role := ""
	for _, row := range parts {
		text.WriteString(row.Content)
		if role == "" {
			role = row.Role
		}
	}
	return text.String(), role
}

// compareStoredParts is a total order over the rows of one message family: the
// part index in the path, then a recorded split position before an unrecorded
// one, then the split position, then the content.
func compareStoredParts(conversationID string, left storedRow, right storedRow) int {
	leftIndex := pathPartIndex(conversationID, left.RelativePath)
	rightIndex := pathPartIndex(conversationID, right.RelativePath)
	if leftIndex != rightIndex {
		return leftIndex - rightIndex
	}
	if left.SplitPartRecorded != right.SplitPartRecorded {
		if left.SplitPartRecorded {
			return -1
		}
		return 1
	}
	if left.SplitPartRecorded && left.SplitPart != right.SplitPart {
		if left.SplitPart < right.SplitPart {
			return -1
		}
		return 1
	}
	return strings.Compare(left.Content, right.Content)
}

// pathPartIndex returns the part number after the message index in a message
// text path, or 0 for a single-part path.
func pathPartIndex(conversationID string, relativePath string) int {
	remainder, found := strings.CutPrefix(relativePath, conversationRelativePathPrefix(conversationID))
	if !found {
		return 0
	}
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 {
		return 0
	}
	partIndex, err := strconv.Atoi(parts[1])
	if err != nil || partIndex < 0 {
		return 0
	}
	return partIndex
}

// assignConversationID returns the requested conversation a stored row belongs
// to. A row with a conversationId column value in the request belongs to that
// conversation. A row without one belongs to the requested conversation with the
// longest matching family path prefix, which covers rows written before the
// column existed.
func assignConversationID(storedID string, relativePath string, requested []string) string {
	if storedID != "" && slices.Contains(requested, storedID) {
		return storedID
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
			if strings.HasPrefix(relativePath, prefix) && len(prefix) > matchedLength {
				matchedID = requestedID
				matchedLength = len(prefix)
			}
		}
	}
	return matchedID
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
func (batch *storedBatch) add(conversationID string, row storedRow, currentModel string) {
	if len(row.Vector) > 0 && embeddingModelsCompatible(row.EmbeddingModel, currentModel) {
		batch.reuse[contentKey(row.Content)] = row.Vector
	}
	if conversationID == "" {
		return
	}
	state, found := batch.conversations[conversationID]
	if !found {
		state = newConversationState()
		batch.conversations[conversationID] = state
	}
	key := chunkFamilyKey(conversationID, row.RelativePath)
	state.families[key] = append(state.families[key], row)
}

func (batch *storedBatch) state(conversationID string) *conversationState {
	return batch.conversations[conversationID]
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
