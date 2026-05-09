// Package sessionrename owns the daemon-side auto-name worker.
//
// The worker watches sessions whose names still look daemon-owned and produces a
// human-readable replacement from transcript content. Claude sessions prefer a
// custom-title header and fall back to the first user message. Codex sessions
// use the shared Codex transcript IR and derive the candidate from the first
// user turn there.
//
// Every applied rename funnels through daemon.Server so the existing live-lease,
// broadcast, and parent-pointer rewriting paths stay authoritative. The worker
// never calls store.Rename directly.
//
// User-owned names are sticky. The daemon records that ownership with
// AutoNameStateUserLocked and AutoNameSourceUser, and the worker permanently
// excludes those sessions from future auto-name passes.
package sessionrename
