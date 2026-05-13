package contextusage

// Snapshot is the provider-neutral payload that mirrors the live
// /context view a provider can expose for a session. Only the fields
// the compact engine needs are modeled; provider-specific
// extensions are intentionally ignored at this boundary so the
// generic engine never depends on a provider-shaped field.
type Snapshot struct {
	Model       string     `json:"model"`
	TotalTokens int        `json:"totalTokens"`
	MaxTokens   int        `json:"maxTokens"`
	Percentage  int        `json:"percentage"`
	Categories  []Category `json:"categories"`
}

// Category mirrors a single /context row. Name is free-form; the
// stable values Claude emits today are "System prompt", "System
// tools", "MCP tools", "Memory files", "Skills", "Custom agents",
// "Messages", "Compact buffer", "Free space", and their
// "(deferred)" variants. Other providers map their own taxonomies
// onto the same shape so generic consumers can stay provider-neutral.
type Category struct {
	Name       string `json:"name"`
	Tokens     int    `json:"tokens"`
	IsDeferred bool   `json:"isDeferred"`
}

// categoryNamesExcludedFromOverhead lists the category names whose
// tokens are NOT part of static overhead. Messages is the transcript
// tail that compact trims. Compact buffer matches the --reserved knob
// and is added back by the planner; including it here would double
// count. Free space is a visualization artifact, not real tokens.
var categoryNamesExcludedFromOverhead = map[string]bool{
	"Messages":       true,
	"Compact buffer": true,
	"Free space":     true,
}

// StaticOverhead derives the non-trimmable floor from the live
// /context total itself. Claude's category rows are not reliably
// additive, especially once deferred buckets are present, so summing
// every non-message category can materially overstate the floor.
// When TotalTokens is present we treat it as the authority and
// subtract the dynamic buckets (Messages, Compact buffer, Free
// space). If total is unavailable, fall back to the older "sum
// included categories" behavior.
func (s Snapshot) StaticOverhead() int {
	excluded := 0
	included := 0
	for _, cat := range s.Categories {
		if categoryNamesExcludedFromOverhead[cat.Name] {
			excluded += cat.Tokens
			continue
		}
		included += cat.Tokens
	}
	if s.TotalTokens > 0 {
		floor := s.TotalTokens - excluded
		if floor < 0 {
			return 0
		}
		return floor
	}
	return included
}
