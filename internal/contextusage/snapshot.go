package contextusage

// Snapshot is the provider-neutral payload that mirrors the live
// context-usage view a provider can expose for a session. Only the
// fields the compact engine needs are modeled; provider-specific
// extensions are intentionally ignored at this boundary so the
// generic engine never depends on a provider-shaped field.
type Snapshot struct {
	Model       string     `json:"model"`
	TotalTokens int        `json:"totalTokens"`
	MaxTokens   int        `json:"maxTokens"`
	Percentage  int        `json:"percentage"`
	Categories  []Category `json:"categories"`
}

// Category mirrors a single provider context-usage row. Name is
// free-form; provider implementations map their own taxonomies onto
// the same shape so generic consumers can stay provider-neutral.
type Category struct {
	Name       string `json:"name"`
	Tokens     int    `json:"tokens"`
	IsDeferred bool   `json:"isDeferred"`
}

// categoryNamesDynamicInTotal lists the categories that are counted in
// TotalTokens and that compact can trim. Messages is the transcript
// tail compact removes, and it is the only consumed bucket the floor
// must exclude. It is subtracted from TotalTokens to derive the floor.
var categoryNamesDynamicInTotal = map[string]bool{
	"Messages": true,
}

// categoryNamesNotInTotal lists categories whose tokens are not part of
// TotalTokens at all. Free space is the unused remainder of the context
// window. Compact buffer is reserved headroom carved out of that same
// remainder. Neither is consumed, so neither is subtracted from the
// total nor summed into the fallback floor. The provider's used-token
// count (TotalTokens) is system prompt, tools, memory, skills, and
// Messages; free space and compact buffer sit outside it.
var categoryNamesNotInTotal = map[string]bool{
	"Compact buffer": true,
	"Free space":     true,
}

// CompactBuffer returns the tokens reserved by the provider as headroom
// for auto-compaction. Sourced from the "Compact buffer" category row
// when present; returns 0 otherwise. Compact uses this as the planner's
// "reserved" floor so its projection matches the provider's actual
// reservation rather than a static guess. The bucket is not part of
// TotalTokens, which is why it gets its own accessor instead of being
// folded into StaticOverhead.
func (s Snapshot) CompactBuffer() int {
	for _, cat := range s.Categories {
		if cat.Name == "Compact buffer" {
			return cat.Tokens
		}
	}
	return 0
}

// StaticOverhead derives the non-trimmable floor from the live context
// total itself. Provider category rows are not guaranteed to be
// additive, especially once deferred buckets are present, so summing
// every non-message category can materially overstate the floor. When
// TotalTokens is present we treat it as the authority and subtract only
// Messages, the one consumed bucket compact trims. Free space and
// compact buffer are not part of TotalTokens, so subtracting them would
// over-remove and drive the floor negative. If total is unavailable,
// fall back to summing the static categories directly.
func (s Snapshot) StaticOverhead() int {
	dynamicInTotal := 0
	includedFloor := 0
	for _, cat := range s.Categories {
		switch {
		case categoryNamesDynamicInTotal[cat.Name]:
			dynamicInTotal += cat.Tokens
		case categoryNamesNotInTotal[cat.Name]:
			// Not part of the total and not part of the floor.
		default:
			includedFloor += cat.Tokens
		}
	}
	if s.TotalTokens > 0 {
		floor := s.TotalTokens - dynamicInTotal
		if floor < 0 {
			return 0
		}
		return floor
	}
	return includedFloor
}
