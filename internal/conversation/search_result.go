package conversation

import (
	"sort"

	"goodkind.io/clyde/internal/homedir"
)

// SearchSource names which engine produced a search result set, so the caller
// can tell a semantic answer from a literal-scan fallback and from a cold index
// that returned nothing on purpose.
type SearchSource uint8

const (
	// SearchSourceUnspecified is the zero value, used before a search resolves
	// its source.
	SearchSourceUnspecified SearchSource = iota
	// SearchSourceSemantic means the vector engine produced the matches.
	SearchSourceSemantic
	// SearchSourceLiteral means the engine was unavailable or empty and the
	// literal transcript scan produced the matches.
	SearchSourceLiteral
	// SearchSourceLiteralDisabledCold means the engine produced nothing and the
	// literal fallback is disabled, so no matches were returned on purpose.
	SearchSourceLiteralDisabledCold
)

// String renders the source as a stable lowercase label for text output.
func (s SearchSource) String() string {
	switch s {
	case SearchSourceSemantic:
		return "semantic"
	case SearchSourceLiteral:
		return "literal"
	case SearchSourceLiteralDisabledCold:
		return "literal_disabled_cold"
	case SearchSourceUnspecified:
		return "unspecified"
	default:
		return "unspecified"
	}
}

// SearchResultOutcome names what an empty cross-conversation search result
// means to callers. Non-empty results are represented by Matches and
// ReturnedCount, so this enum stays focused on the cold and empty cases.
type SearchResultOutcome uint8

const (
	// SearchResultOutcomeDidNotLook means no search was actually performed.
	SearchResultOutcomeDidNotLook SearchResultOutcome = iota
	// SearchResultOutcomeNoMatches means search ran and found no matches.
	SearchResultOutcomeNoMatches
	// SearchResultOutcomeNotIndexed means the queried scope is not searchable yet.
	SearchResultOutcomeNotIndexed
)

// String renders the outcome as a stable uppercase label for human and agent
// output.
func (o SearchResultOutcome) String() string {
	switch o {
	case SearchResultOutcomeNotIndexed:
		return "NOT_INDEXED"
	case SearchResultOutcomeNoMatches:
		return "NO_MATCHES"
	case SearchResultOutcomeDidNotLook:
		return "DID_NOT_LOOK"
	default:
		return "DID_NOT_LOOK"
	}
}

// SearchResultOutcomeFromSource maps the current implementation source signal
// onto caller-meaningful empty-result states.
func SearchResultOutcomeFromSource(source SearchSource) SearchResultOutcome {
	switch source {
	case SearchSourceLiteralDisabledCold:
		return SearchResultOutcomeNotIndexed
	case SearchSourceSemantic, SearchSourceLiteral:
		// TODO: Replace this source-only stub once the engine reports whether an
		// empty response means the queried scope is unindexed or genuinely has no
		// matches.
		return SearchResultOutcomeNoMatches
	case SearchSourceUnspecified:
		return SearchResultOutcomeDidNotLook
	default:
		return SearchResultOutcomeDidNotLook
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
