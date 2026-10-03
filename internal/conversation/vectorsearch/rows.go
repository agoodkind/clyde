package vectorsearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

// conversationChunkMaxBytes is the byte budget that splits one message field
// into several rows before the embedding token cap applies.
const conversationChunkMaxBytes = 60000

// storedChunk is one conversation row before embedding. The lm-semantic-search
// daemon wrote the existing rows, and each derivation below matches its row
// construction. The struct omits startLine, endLine, and fileExtension, which
// are always 0, 0, and empty.
type storedChunk struct {
	Content              string
	RelativePath         string
	ConversationID       string
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	WorkspaceRoot        string
	Archived             bool
	SplitPart            int32
	LoadRules            string
}

// chunkFamily is the rows one message part produces: a message text, one tool
// call, or one thinking block. Key is the family path without a part suffix.
// Chunks are the family's physical rows.
type chunkFamily struct {
	Key    string
	Chunks []storedChunk
}

// conversationRelativePath returns the relativePath of a message text row.
func conversationRelativePath(conversationID string, messageIndex int32, partIndex int, multipart bool) string {
	basePath := fmt.Sprintf("conv/%s/%d", conversationID, messageIndex)
	if !multipart {
		return basePath
	}
	return fmt.Sprintf("%s/%d", basePath, partIndex)
}

func conversationRelativePathPrefix(conversationID string) string {
	return "conv/" + conversationID + "/"
}

func conversationToolRelativePathPrefix(conversationID string) string {
	return "convtool/" + conversationID + "/"
}

func conversationThinkingRelativePathPrefix(conversationID string) string {
	return "convthink/" + conversationID + "/"
}

func conversationToolCallPath(conversationID string, messageIndex int32, toolIndex int) string {
	return fmt.Sprintf("convtool/%s/%d/%d", conversationID, messageIndex, toolIndex)
}

func conversationThinkingPath(conversationID string, messageIndex int32) string {
	return fmt.Sprintf("convthink/%s/%d", conversationID, messageIndex)
}

// providerFromConversationID returns the prefix of a conversation ID before the
// first colon, or empty when the ID has no provider prefix.
func providerFromConversationID(conversationID string) string {
	separator := strings.IndexByte(conversationID, ':')
	if separator <= 0 {
		return ""
	}
	return conversationID[:separator]
}

// conversationTextIsStorable reports whether content has a character other than
// whitespace. A field with none stores no row.
func conversationTextIsStorable(text string) bool {
	return strings.TrimSpace(text) != ""
}

// splitTextByBytes cuts text into UTF-8 aligned pieces of at most maxBytes. A
// non-positive maxBytes returns the text whole.
func splitTextByBytes(text string, maxBytes int) []string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return []string{text}
	}
	pieces := make([]string, 0, (len(text)+maxBytes-1)/maxBytes)
	start := 0
	for start < len(text) {
		end := start + maxBytes
		if end >= len(text) {
			pieces = append(pieces, text[start:])
			break
		}
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == start {
			_, size := utf8.DecodeRuneInString(text[start:])
			end = start + size
		}
		pieces = append(pieces, text[start:end])
		start = end
	}
	return pieces
}

type chunkBuilder func(piece string, partIndex int, multipart bool) storedChunk

// appendStorableField splits one field and appends a row for every piece. It
// appends nothing when the field has no storable text. The decision covers the
// whole field, because a message's stored text is the concatenation of its
// pieces.
func appendStorableField(chunks []storedChunk, content string, budget int, build chunkBuilder) []storedChunk {
	if !conversationTextIsStorable(content) {
		return chunks
	}
	pieces := splitTextByBytes(content, budget)
	multipart := len(pieces) > 1
	for partIndex, piece := range pieces {
		chunks = append(chunks, build(piece, partIndex, multipart))
	}
	return chunks
}

// appendContinuedField appends the rows of one field like appendStorableField
// and starts every part after the first with continuationPrefix and a newline.
// A non-empty prefix lowers the budget by its length plus one when that leaves a
// positive budget.
func appendContinuedField(
	chunks []storedChunk,
	content string,
	budget int,
	continuationPrefix string,
	build chunkBuilder,
) []storedChunk {
	if continuationPrefix != "" && budget > len(continuationPrefix)+1 {
		budget -= len(continuationPrefix) + 1
	}
	return appendStorableField(chunks, content, budget, func(piece string, partIndex int, multipart bool) storedChunk {
		if partIndex > 0 && continuationPrefix != "" {
			piece = continuationPrefix + "\n" + piece
		}
		return build(piece, partIndex, multipart)
	})
}

// toolContent returns the searchable text of one tool call: its name and its
// display text, one token per line, skipping an empty or repeated token. The
// lm-semantic-search daemon also decomposed a bash command into program names
// and file targets with a parser that Clyde does not depend on. A bash command
// here contributes its display text only, the same text the daemon wrote for a
// command its parser could not decompose.
func toolContent(tool semsearch.SemToolCall) string {
	tokens := make([]string, 0, 2)
	tokens = appendToken(tokens, tool.Name)
	tokens = appendToken(tokens, tool.Display)
	return strings.Join(tokens, "\n")
}

func appendToken(tokens []string, value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return tokens
	}
	if slices.Contains(tokens, trimmed) {
		return tokens
	}
	return append(tokens, trimmed)
}

func newChunk(doc semsearch.SemDoc, conversationID string, parentConversationID string, relativePath string, content string) storedChunk {
	return storedChunk{
		Content:              content,
		RelativePath:         relativePath,
		ConversationID:       conversationID,
		ParentConversationID: parentConversationID,
		MessageIndex:         doc.MessageIndex,
		Role:                 doc.Role,
		TimestampUnix:        doc.TimestampUnix,
		WorkspaceRoot:        doc.WorkspaceRoot,
		Archived:             doc.Archived,
		SplitPart:            0,
		LoadRules:            doc.LoadRules,
	}
}

// documentChunks returns the rows of one message document: the message text, one
// row group per tool call, and the thinking block, each split at budget bytes.
func documentChunks(doc semsearch.SemDoc, budget int) ([]storedChunk, error) {
	conversationID := strings.TrimSpace(doc.ConversationID)
	if conversationID == "" {
		return nil, errors.New("conversation id is required")
	}
	parentConversationID := strings.TrimSpace(doc.ParentConversationID)
	chunks := appendStorableField(nil, doc.Text, budget, func(piece string, partIndex int, multipart bool) storedChunk {
		path := conversationRelativePath(conversationID, doc.MessageIndex, partIndex, multipart)
		return newChunk(doc, conversationID, parentConversationID, path, piece)
	})
	for toolIndex, tool := range doc.Tools {
		cleaned := semsearch.SemToolCall{
			Name:     strings.ToValidUTF8(tool.Name, ""),
			Display:  strings.ToValidUTF8(tool.Display, ""),
			LangHint: strings.ToValidUTF8(tool.LangHint, ""),
			Output:   strings.ToValidUTF8(tool.Output, ""),
			IsError:  tool.IsError,
		}
		basePath := conversationToolCallPath(conversationID, doc.MessageIndex, toolIndex)
		chunks = appendContinuedField(chunks, toolContent(cleaned), budget, strings.TrimSpace(cleaned.Name),
			func(piece string, partIndex int, multipart bool) storedChunk {
				path := basePath
				if multipart {
					path = fmt.Sprintf("%s/%d", basePath, partIndex)
				}
				return newChunk(doc, conversationID, parentConversationID, path, piece)
			})
	}
	thinkingPath := conversationThinkingPath(conversationID, doc.MessageIndex)
	chunks = appendStorableField(chunks, strings.ToValidUTF8(doc.Thinking, ""), budget,
		func(piece string, partIndex int, multipart bool) storedChunk {
			path := thinkingPath
			if multipart {
				path = fmt.Sprintf("%s/%d", thinkingPath, partIndex)
			}
			return newChunk(doc, conversationID, parentConversationID, path, piece)
		})
	return chunks, nil
}

// chunkFamilyKey strips the part suffix from a conversation row path. A message
// text family is conv/<id>/<message>, a tool call family is
// convtool/<id>/<message>/<tool>, and a thinking family is
// convthink/<id>/<message>.
func chunkFamilyKey(conversationID string, relativePath string) string {
	families := []struct {
		prefix   string
		segments int
	}{
		{prefix: conversationRelativePathPrefix(conversationID), segments: 1},
		{prefix: conversationToolRelativePathPrefix(conversationID), segments: 2},
		{prefix: conversationThinkingRelativePathPrefix(conversationID), segments: 1},
	}
	for _, family := range families {
		remainder, found := strings.CutPrefix(relativePath, family.prefix)
		if !found {
			continue
		}
		parts := strings.Split(remainder, "/")
		if len(parts) <= family.segments {
			return relativePath
		}
		return family.prefix + strings.Join(parts[:family.segments], "/")
	}
	return relativePath
}

// groupFamilies groups one conversation's rows by family in row order.
func groupFamilies(conversationID string, chunks []storedChunk) []chunkFamily {
	families := make([]chunkFamily, 0)
	positions := make(map[string]int)
	for _, chunk := range chunks {
		key := chunkFamilyKey(conversationID, chunk.RelativePath)
		position, found := positions[key]
		if !found {
			position = len(families)
			positions[key] = position
			families = append(families, chunkFamily{Key: key, Chunks: nil})
		}
		families[position].Chunks = append(families[position].Chunks, chunk)
	}
	return families
}

// chunkID returns the primary key of a row. The key hashes the relative path,
// the zero line range, the split position when it is positive, and the content.
func chunkID(chunk storedChunk) string {
	hashInput := fmt.Sprintf("%s:%d:%d:%s", chunk.RelativePath, 0, 0, chunk.Content)
	if chunk.SplitPart > 0 {
		hashInput = fmt.Sprintf("%s:%d:%d:%d:%s", chunk.RelativePath, 0, 0, chunk.SplitPart, chunk.Content)
	}
	sum := sha256.Sum256([]byte(hashInput))
	return "chunk_" + hex.EncodeToString(sum[:])[:16]
}

// chunkMetadataJSON is the metadata JSON the lm-semantic-search daemon stored
// on every conversation row.
type chunkMetadataJSON struct {
	Language             string `json:"language,omitempty"`
	ConversationID       string `json:"conversation_id,omitempty"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	MessageIndex         *int32 `json:"message_index,omitempty"`
	Role                 string `json:"role,omitempty"`
	TimestampUnix        *int64 `json:"timestamp_unix,omitempty"`
}

// encodeChunkMetadata returns the metadata JSON of a row. Every conversation
// row has a conversation ID, so message_index and timestamp_unix are always
// present.
func encodeChunkMetadata(chunk storedChunk) string {
	messageIndex := chunk.MessageIndex
	timestampUnix := chunk.TimestampUnix
	metadata := chunkMetadataJSON{
		Language:             "",
		ConversationID:       chunk.ConversationID,
		ParentConversationID: chunk.ParentConversationID,
		MessageIndex:         &messageIndex,
		Role:                 chunk.Role,
		TimestampUnix:        &timestampUnix,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// chunkScalars returns the value of every conversation scalar column for a row.
// The role is lowercased and the provider comes from the conversation ID prefix.
func chunkScalars(chunk storedChunk) map[string]collection.ScalarValue {
	return map[string]collection.ScalarValue{
		conversationIDColumn:       collection.StringScalar(chunk.ConversationID),
		parentConversationIDColumn: collection.StringScalar(chunk.ParentConversationID),
		roleColumn:                 collection.StringScalar(strings.ToLower(chunk.Role)),
		providerColumn:             collection.StringScalar(providerFromConversationID(chunk.ConversationID)),
		workspaceRootColumn:        collection.StringScalar(chunk.WorkspaceRoot),
		archivedColumn:             collection.BoolScalar(chunk.Archived),
		timestampUnixColumn:        collection.Int64Scalar(chunk.TimestampUnix),
		messageIndexColumn:         collection.Int64Scalar(int64(chunk.MessageIndex)),
		loadRulesColumn:            collection.StringScalar(chunk.LoadRules),
	}
}

// chunkRow converts an embedded chunk to the row the store writes. The row has
// startLine 0, endLine 0, an empty fileExtension, and a recorded splitPart.
func chunkRow(chunk storedChunk, vector []float32) collection.Row {
	return collection.Row{
		ID:                chunkID(chunk),
		Content:           chunk.Content,
		RelativePath:      chunk.RelativePath,
		StartLine:         0,
		EndLine:           0,
		FileExtension:     "",
		Metadata:          encodeChunkMetadata(chunk),
		SplitPart:         chunk.SplitPart,
		SplitPartRecorded: true,
		Vector:            vector,
		Scalars:           chunkScalars(chunk),
	}
}
