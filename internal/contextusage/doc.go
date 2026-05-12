// Package contextusage models a provider-neutral snapshot of the
// live "what does /context show right now" answer for a session. The
// generic compact engine and any caller that reasons about static
// overhead, message tail tokens, or context budgets depends on this
// package. Provider-specific probe machinery (Claude's spawn-and-pipe
// SDK control request, future Codex equivalents) lives in the
// provider package and registers a Prober via Register at startup,
// keeping the dependency flowing from provider to generic.
package contextusage
