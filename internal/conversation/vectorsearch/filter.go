package vectorsearch

import (
	"strings"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

// filterDimensionCount is the capacity of the filter child list.
const filterDimensionCount = 10

// searchFilter converts a conversation search filter to the typed filter tree
// over the conversation columns. It returns nil when no dimension is set, which
// searches the whole collection. Every set dimension becomes one child of a
// top-level all node. Role values are lowercased to match the lowercased role
// column. From bounds are inclusive and until bounds are exclusive. MinScore is
// not part of the tree; the search applies it as a floor after ranking.
func searchFilter(filter semsearch.SearchFilter) *collection.Filter {
	children := make([]collection.Filter, 0, filterDimensionCount)
	if len(filter.Providers) > 0 {
		children = append(children, collection.ColumnIn(providerColumn, collection.StringValues(filter.Providers)))
	}
	if len(filter.WorkspaceRoots) > 0 {
		children = append(children, collection.ColumnIn(workspaceRootColumn, collection.StringValues(filter.WorkspaceRoots)))
	}
	if len(filter.Roles) > 0 {
		children = append(children, collection.ColumnIn(roleColumn, collection.StringValues(lowercaseAll(filter.Roles))))
	}
	if len(filter.ConversationIDs) > 0 {
		children = append(children, collection.ColumnIn(conversationIDColumn, collection.StringValues(filter.ConversationIDs)))
	}
	if filter.ParentConversationID != "" {
		children = append(children, collection.ColumnEquals(parentConversationIDColumn, collection.StringScalar(filter.ParentConversationID)))
	}
	if filter.FromUnix > 0 {
		lower := filter.FromUnix
		children = append(children, collection.ColumnRange(timestampUnixColumn, &lower, nil))
	}
	if filter.UntilUnix > 0 {
		upper := filter.UntilUnix
		children = append(children, collection.ColumnRange(timestampUnixColumn, nil, &upper))
	}
	if filter.MessageIndexFrom > 0 {
		lower := int64(filter.MessageIndexFrom)
		children = append(children, collection.ColumnRange(messageIndexColumn, &lower, nil))
	}
	if filter.MessageIndexUntil > 0 {
		upper := int64(filter.MessageIndexUntil)
		children = append(children, collection.ColumnRange(messageIndexColumn, nil, &upper))
	}
	if len(children) == 0 {
		return nil
	}
	tree := collection.AllOf(children...)
	return &tree
}

func lowercaseAll(values []string) []string {
	lowered := make([]string, 0, len(values))
	for _, value := range values {
		lowered = append(lowered, strings.ToLower(value))
	}
	return lowered
}
