// Package sessionrename owns the daemon-side auto-rename apply
// engine. The worker watches sessions whose AutoNameSource is empty
// or default and produces a kebab-case replacement from the trimmed
// first user message. The LLM-backed source lands in PR5; this
// package ships the transcript-only candidate path.
//
// Every applied rename funnels through the supplied Renamer, which
// the daemon implements on Server so the live-lease gate and the
// existing KIND_SESSION_RENAMED broadcast stay authoritative. The
// worker never calls store.Rename directly.
//
// The user-owned predicate excludes any session with
// AutoNameSource = user or AutoNameState = user_locked. The daemon's
// rename modal callback records AutoNameSourceUser whenever the user
// types a name, so a user-typed name is permanently locked from
// future auto-rename. PR2 is responsible for that callback wiring;
// when PR2 has not landed, the worker still excludes the session as
// soon as the modal records the source.
//
// Cooldown, dedupe, and rate-limit state live on Metadata so they
// survive daemon reload without any in-memory carryover. The hash
// of the redacted source text is the AutoNameSourceHash dedupe
// signal; cooldown uses LastAutoNameAt; the global rate limiter is
// the only piece of in-memory state and resets on reload by design.
package sessionrename
