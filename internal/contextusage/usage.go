package contextusage

import "time"

// Usage is the authoritative answer to "what does /context show for
// this session right now." The embedded Snapshot carries the
// provider-neutral shape; CapturedAt and Source are layer metadata so
// loggers can tell a cache hit from a just-spawned probe. Lives in the
// generic package because every field is provider-neutral; provider
// implementations populate it but do not need to extend it.
type Usage struct {
	Snapshot

	CapturedAt time.Time `json:"captured_at"`
	Source     Source    `json:"source"`
}

// StaticOverhead returns the non-trimmable /context floor for the
// session, delegating to the embedded Snapshot so the formula lives
// in one place.
func (u Usage) StaticOverhead() int {
	return u.Snapshot.StaticOverhead()
}

// TailTokens returns tokens attributable to post-boundary messages,
// as the provider's /context view reports them. This is what compact
// trims.
func (u Usage) TailTokens() int {
	return u.CategoryTokens("Messages")
}

// CategoryTokens returns the token count for the named category, or
// zero when the category is absent. Match is exact; callers use the
// stable category names the provider emits in its /context payload.
func (u Usage) CategoryTokens(name string) int {
	total := 0
	for _, cat := range u.Categories {
		if cat.Name == name {
			total += cat.Tokens
		}
	}
	return total
}
