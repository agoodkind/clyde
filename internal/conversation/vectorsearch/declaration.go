// Package vectorsearch searches the Milvus conversation collection in process
// through the lm-semantic-search collection library. It owns the conversation
// column declaration, the collection naming rule, the filter mapping, and the
// conversion of ranked rows to conversation hits.
package vectorsearch

import (
	"encoding/hex"
	"strings"

	"goodkind.io/lm-semantic-search/collection"
)

// Column names of the conversation collection. Rows written by the
// lm-semantic-search daemon use these names, so they cannot change.
const (
	conversationIDColumn       = "conversationId"
	parentConversationIDColumn = "parentConversationId"
	roleColumn                 = "role"
	providerColumn             = "provider"
	workspaceRootColumn        = "workspaceRoot"
	archivedColumn             = "archived"
	timestampUnixColumn        = "timestampUnix"
	messageIndexColumn         = "messageIndex"
	loadRulesColumn            = "loadRules"
)

// Maximum lengths of the string columns, matching the stored Milvus schema.
const (
	conversationIDMaxLength = 256
	roleMaxLength           = 64
	providerMaxLength       = 32
	workspaceRootMaxLength  = 1024
	loadRulesMaxLength      = 256
)

const (
	collectionNamePrefix = "conv_chunks_"
	// collectionNameHashChars is how many leading hex characters of the collection
	// ID digest follow the prefix.
	collectionNameHashChars = 8
)

// Declaration returns the scalar declaration of the conversation collection.
// conversationId stores the item ID. Every column is nullable, because rows
// written before a column existed read null for it.
func Declaration() collection.Declaration {
	return collection.Declaration{
		ItemIDColumn: conversationIDColumn,
		Scalars: []collection.ScalarColumn{
			nullableStringColumn(conversationIDColumn, conversationIDMaxLength),
			nullableStringColumn(parentConversationIDColumn, conversationIDMaxLength),
			nullableStringColumn(roleColumn, roleMaxLength),
			nullableStringColumn(providerColumn, providerMaxLength),
			nullableStringColumn(workspaceRootColumn, workspaceRootMaxLength),
			{Name: archivedColumn, Type: collection.ScalarTypeBool, Nullable: true, MaxLength: 0},
			{Name: timestampUnixColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0},
			{Name: messageIndexColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0},
			nullableStringColumn(loadRulesColumn, loadRulesMaxLength),
		},
	}
}

func nullableStringColumn(name string, maxLength int32) collection.ScalarColumn {
	return collection.ScalarColumn{
		Name:      name,
		Type:      collection.ScalarTypeString,
		Nullable:  true,
		MaxLength: maxLength,
	}
}

// CollectionName returns the Milvus collection that stores the conversations of
// collectionID: the conv_chunks_ prefix plus the first 8 hex characters of the
// MD5 digest of the trimmed ID.
func CollectionName(collectionID string) string {
	digest := md5Sum([]byte(strings.TrimSpace(collectionID)))
	return collectionNamePrefix + hex.EncodeToString(digest[:])[:collectionNameHashChars]
}
