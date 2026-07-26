package conversation

import (
	"sort"

	"goodkind.io/clyde/internal/homedir"
)

// SearchSource names the provider that produced a search result set.
type SearchSource uint8

const (
	// SearchSourceUnspecified is the zero value, used before a search resolves
	// its source.
	SearchSourceUnspecified SearchSource = iota
	// SearchSourceSemantic means the vector engine produced the matches.
	SearchSourceSemantic
)

// String renders the source as a stable lowercase label for text output.
func (s SearchSource) String() string {
	switch s {
	case SearchSourceSemantic:
		return "semantic"
	case SearchSourceUnspecified:
		return "unspecified"
	default:
		return "unspecified"
	}
}

// SearchFacetCount is one facet value and how many matched conversations carry
// it.
type SearchFacetCount struct {
	Value string
	Count int
}

// SearchFacets summarizes the match set by workspace, provider, and model so the
// caller can narrow a follow-up query without a second pass.
type SearchFacets struct {
	Workspaces []SearchFacetCount
	Providers  []SearchFacetCount
	Models     []SearchFacetCount
}

// SearchFreshness is the conversation-index sync state at query time, so the
// caller can tell whether a thin result means a cold index rather than a true
// miss.
type SearchFreshness struct {
	Manifest     int
	Needed       int
	Embedded     int
	Pending      int
	LastSyncUnix int64
}

// FilterStage records how many candidate conversations remained after one
// filter was applied, so a legitimately narrow result shows its funnel.
type FilterStage struct {
	Name      string
	Remaining int
}

// ComputeFacets tallies the match set into the top-N values per dimension by
// count, ordered count descending then value ascending so the output is
// deterministic. Workspace values are contracted to their display form.
func ComputeFacets(matches []SearchMatch, topN int) SearchFacets {
	workspaceCounts := map[string]int{}
	providerCounts := map[string]int{}
	modelCounts := map[string]int{}
	for _, match := range matches {
		workspaceCounts[homedir.Contract(match.Record.WorkspaceRoot)]++
		providerCounts[match.Record.Provider.String()]++
		modelCounts[match.Record.Model]++
	}
	return SearchFacets{
		Workspaces: topFacetCounts(workspaceCounts, topN),
		Providers:  topFacetCounts(providerCounts, topN),
		Models:     topFacetCounts(modelCounts, topN),
	}
}

// topFacetCounts converts a value-count map into the top-N counts, dropping the
// empty value and ordering by count descending then value ascending.
func topFacetCounts(counts map[string]int, topN int) []SearchFacetCount {
	ordered := make([]SearchFacetCount, 0, len(counts))
	for value, count := range counts {
		if value == "" {
			continue
		}
		ordered = append(ordered, SearchFacetCount{Value: value, Count: count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Count != ordered[j].Count {
			return ordered[i].Count > ordered[j].Count
		}
		return ordered[i].Value < ordered[j].Value
	})
	if topN > 0 && len(ordered) > topN {
		ordered = ordered[:topN]
	}
	return ordered
}
