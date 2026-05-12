package contextusage

import (
	"time"

	"goodkind.io/clyde/internal/contextusage"
)

// Source re-exports the generic provider-neutral Source so existing
// callers in this package can keep referencing a Source value without
// importing the generic package directly.
type Source = contextusage.Source

// Source constants alias the generic ones so call sites that pass
// SourceProbe/SourceCacheMem/SourceCacheDisk keep compiling.
const (
	SourceProbe     = contextusage.SourceProbe
	SourceCacheMem  = contextusage.SourceCacheMem
	SourceCacheDisk = contextusage.SourceCacheDisk
)

// Category mirrors Claude Code's /context row. The alias points at
// the generic contextusage.Category so the provider-neutral package
// owns the shape and the Claude package adapts to it.
type Category = contextusage.Category

// Usage is the authoritative answer to "what does /context show for
// this session right now." The embedded Snapshot carries the
// provider-neutral shape; CapturedAt and Source are layer metadata
// so loggers can tell a 30s-old cache hit from a just-spawned probe.
type Usage struct {
	contextusage.Snapshot

	CapturedAt time.Time `json:"captured_at"`
	Source     Source    `json:"source"`
}

// StaticOverhead returns the non-trimmable /context floor for the
// session. It delegates to the embedded Snapshot so the formula
// lives in one place in the generic package.
func (u Usage) StaticOverhead() int {
	return u.Snapshot.StaticOverhead()
}

// TailTokens returns the tokens attributable to post-boundary
// messages, as Claude reports them. This is what compact trims.
func (u Usage) TailTokens() int {
	return u.CategoryTokens("Messages")
}

// CategoryTokens returns the token count for the named category, or
// zero when the category is absent from the response. The match is
// exact; Claude's stable names are surfaced as-is ("System prompt",
// "System tools", "MCP tools", "Memory files", "Skills", "Messages",
// "Compact buffer", "Free space", plus deferred variants suffixed
// " (deferred)").
func (u Usage) CategoryTokens(name string) int {
	total := 0
	for _, cat := range u.Categories {
		if cat.Name == name {
			total += cat.Tokens
		}
	}
	return total
}
