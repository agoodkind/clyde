package daemon

import (
	"path/filepath"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

// embeddedConversationFilter evaluates policy against committed scalars rather
// than the current raw index, which can omit retained or hidden artifacts.
func embeddedConversationFilter(semantic config.ConversationSemanticConfig, options conversation.SearchConversationsOptions) (*library.Filter, error) {
	kinds, err := SemanticContentKinds(semantic)
	if err != nil {
		return nil, err
	}
	children := []library.Filter{{
		Op: library.Equal, Column: embeddedScalarProjectionProfile,
		Values: []library.ScalarValue{embeddedStringScalar(searchbackend.ProjectionProfile(conversation.LoadRulesTag(kinds)))},
	}}
	if !semantic.IncludeArchived && !options.IncludeArchived {
		children = append(children, embeddedEqualFilter(embeddedScalarArchived, embeddedBoolScalar(false)))
	}
	if !semantic.IncludeSubagents {
		children = append(children, embeddedEqualFilter(embeddedScalarSubagent, embeddedBoolScalar(false)))
	}
	if len(semantic.IndexedProviders) > 0 {
		children = append(children, embeddedStringSetFilter(embeddedScalarProvider, semantic.IndexedProviders))
	}
	if options.Provider.Valid() {
		children = append(children, embeddedEqualFilter(embeddedScalarProvider, embeddedStringScalar(options.Provider.String())))
	}
	if len(semantic.IndexedRoles) > 0 {
		children = append(children, embeddedStringSetFilter(embeddedScalarRole, semantic.IndexedRoles))
	}
	if len(options.Roles) > 0 {
		children = append(children, embeddedStringSetFilter(embeddedScalarRole, options.Roles))
	}
	if options.ConversationID != "" {
		children = append(children, embeddedStringSetFilter(embeddedScalarConversationID, []string{options.ConversationID}))
	}
	if workspace := strings.TrimSpace(options.WorkspaceRoot); workspace != "" {
		workspace = filepath.Clean(workspace)
		// A root includes descendants, but not a sibling with the same prefix.
		children = append(children, library.Filter{Op: library.Any, Children: []library.Filter{
			embeddedEqualFilter(embeddedScalarWorkspaceRoot, embeddedStringScalar(workspace)),
			{Op: library.Prefix, Column: embeddedScalarWorkspaceRoot, Prefix: strings.TrimRight(workspace, string(filepath.Separator)) + string(filepath.Separator)},
		}})
	}
	if options.FromUnix != 0 || options.UntilUnix != 0 {
		rangeFilter := library.Filter{Op: library.Range, Column: embeddedScalarTimestampUnix}
		if options.FromUnix != 0 {
			lower := embeddedInt64Scalar(options.FromUnix)
			rangeFilter.Lower = &lower
		}
		if options.UntilUnix != 0 {
			upper := embeddedInt64Scalar(options.UntilUnix)
			rangeFilter.Upper = &upper
		}
		children = append(children, rangeFilter)
	}
	return &library.Filter{Op: library.All, Children: children}, nil
}

func embeddedEqualFilter(column string, value library.ScalarValue) library.Filter {
	return library.Filter{Op: library.Equal, Column: column, Values: []library.ScalarValue{value}}
}

func embeddedStringSetFilter(column string, values []string) library.Filter {
	scalars := make([]library.ScalarValue, 0, len(values))
	for _, value := range values {
		scalars = append(scalars, embeddedStringScalar(value))
	}
	return library.Filter{Op: library.In, Column: column, Values: scalars}
}
