package contextusage

import "goodkind.io/clyde/internal/categorystyle"

// claudeCategoryStyleID names this provider in the categorystyle
// registry. It matches session.ProviderClaude but is duplicated here
// to keep the dependency direction one-way.
const claudeCategoryStyleID = "claude"

// claudeCategoryColors maps stable Claude /context category names to
// color hints. Today the mapping is empty: no concrete color values
// have been collected from the Claude UI, and the dashboard has its
// own fallback palette for un-registered names. Populating this map
// later is a pure provider concern and does not touch generic code.
//
// TODO(CLYDE-393): collect the actual Claude /context color tokens
// (Messages, Free space, Compact buffer, System prompt, System tools,
// MCP tools, Memory files, Skills, Custom agents, and the
// "(deferred)" variants) and register them here. Until then,
// consumers fall back to their built-in palette.
var claudeCategoryColors = map[string]categorystyle.Color{}

func init() {
	categorystyle.Register(claudeCategoryStyleID, claudeCategoryColors)
}
