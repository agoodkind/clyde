package conversation

import "strings"

// A load-rules tag names the loading rules that produced a stored search row's
// message index, so a later reader can rebuild the same message sequence the
// index refers to. The feeder writes the tag on every document it delivers, the
// engine stores it per row and returns it on search hits, and the context
// window reader maps it back onto the load gate. The store is append-only, so
// a row's tag is the only durable record of the rules it was written under.
//
// The tag is versioned. Version v1 lists the included load-gated kinds by
// selector name, so a kind added later is absent from old tags and correctly
// reads as excluded. A change that alters what an existing kind gates cannot
// be expressed by the name list and must ship as a new version, keeping v1
// rows interpreted under v1 meaning.
const loadRulesTagVersionV1 = "v1;"

// loadGatedContentKinds lists the content kinds whose selection changes which
// records the parser yields, in canonical order. Only these kinds shift
// message indices; every other kind changes what a document contains, never
// which messages occupy positions.
var loadGatedContentKinds = []ContentKind{
	ContentKindToolOutputs,
	ContentKindSystemPrompts,
	ContentKindSystemMessages,
	ContentKindInjected,
}

// LoadRulesTag renders the tag for the given kind set: the v1 version prefix
// followed by the included load-gated kinds, comma-joined in canonical order.
// The default kind set yields "v1;" with an empty list, which is distinct from
// the empty string an untagged legacy row carries.
func LoadRulesTag(kinds ContentKindSet) string {
	included := make([]string, 0, len(loadGatedContentKinds))
	for _, kind := range loadGatedContentKinds {
		if kinds.Has(kind) {
			included = append(included, string(kind))
		}
	}
	return loadRulesTagVersionV1 + strings.Join(included, ",")
}

// LoadOptionsForRules maps a stored tag back onto the parser's load gate.
// The empty tag is a legacy row written before tagging existed; every such row
// was written under the default rules, so it maps to the default gate. A tag
// with an unrecognized version was written by a newer sender; it also maps to
// the default gate, and known reports false so the caller can log the
// degraded read instead of failing it.
func LoadOptionsForRules(tag string) (options LoadOptions, known bool) {
	defaultOptions := LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          nil,
	}
	if tag == "" {
		return defaultOptions, true
	}
	list, versioned := strings.CutPrefix(tag, loadRulesTagVersionV1)
	if !versioned {
		return defaultOptions, false
	}
	options = defaultOptions
	for name := range strings.SplitSeq(list, ",") {
		switch ContentKind(strings.TrimSpace(name)) {
		case ContentKindToolOutputs:
			options.IncludeToolOutputs = true
		case ContentKindSystemPrompts:
			options.IncludeSystemPrompts = true
		case ContentKindSystemMessages:
			options.IncludeSystemMessages = true
		case ContentKindInjected:
			options.IncludeInjected = true
		case ContentKindChat, ContentKindThinking, ContentKindToolSummaries,
			ContentKindToolCalls, ContentKindRawJSONMetadata:
			// Non-gated kinds never appear in a tag this build wrote, and one
			// in a foreign tag has no load gate to set, so it is ignored.
		}
	}
	return options, true
}
