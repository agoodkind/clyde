package contextusage

import (
	"context"
	"time"

	"goodkind.io/clyde/internal/compact"
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

// UsageOptions controls a single Layer.Usage call. Zero values
// produce cache-preferred behavior with the layer's default TTL.
type UsageOptions struct {
	// Refresh forces a fresh probe and busts both cache tiers. Use for
	// calibration, where an outdated static_overhead would persist.
	Refresh bool

	// MaxAge caps acceptable cache age. A zero value accepts any hit
	// within the layer's configured TTL. Stricter MaxAge lets callers
	// who care about freshness (TUI context meter) narrow the window
	// without opting into a full refresh.
	MaxAge time.Duration
}

// CountOptions controls a single Layer.Count call. Zero values use
// the layer's configured default model.
type CountOptions struct {
	// Model overrides the layer's default model for this call. Used
	// by callers that count payloads targeted at a specific model and
	// want the honest count for that tokenizer.
	Model string
}

// Layer is the one entry point every session-context caller uses. The
// two methods answer orthogonal questions: Usage is session-wide and
// matches /context exactly; Count is payload-sized and routes to
// Anthropic's count_tokens endpoint.
type Layer interface {
	// Usage returns the live Claude /context for the session. The
	// probe backend is the truth source. Cache tiers serve previous
	// probe results when they satisfy opts.MaxAge and the transcript
	// has not been written since capture. The returned value is by
	// construction identical to what /context prints inside the chat.
	Usage(ctx context.Context, opts UsageOptions) (Usage, error)

	// Count returns Anthropic's count_tokens for a synthetic user
	// message whose content is the supplied block array. Used by the
	// planner target loop, where the payload is a compaction
	// candidate that does not exist on disk yet.
	Count(ctx context.Context, content []compact.OutputBlock, opts CountOptions) (int, error)

	// SessionID returns the UUID this layer is bound to. Callers use
	// it for log correlation when they already hold the layer but
	// not the session reference.
	SessionID() string
}
