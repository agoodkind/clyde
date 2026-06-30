// Package livetrack is the universal session registry that tracks
// long-lived state crossing reload, shutdown, and force-close
// boundaries inside Clyde subsystems. It is intentionally a leaf
// package: it depends only on the standard library and
// internal/correlation so that any subsystem (MITM tunnels, gRPC
// streams, provider websockets, MCP stdio handlers, browser tabs,
// file watchers, async workers) can adopt it without dragging in
// other Clyde dependencies.
//
// The Meta type parameter on Registry, Session, and Predicate carries
// per-subsystem metadata. There is no shared concrete Meta in this
// package; each adopter declares its own struct and instantiates
// Registry[ItsMeta]. This keeps livetrack neutral about subsystem
// concerns while making registry snapshots strongly typed at the
// call site.
//
// Adopters should construct one Registry per subsystem, register
// every long-lived owner with a Closer that releases the owner's OS
// resources (sockets, fds, flocks, hijacked conns), and call Drain
// during shutdown or reload so the daemon has a single inventory of
// what is still alive and a bounded force-close path when graceful
// drain hits its deadline.
package livetrack

// Meta is the marker constraint for per-subsystem session metadata.
// Adopters declare a struct (e.g. MITMMeta) and add a no-op
// IsLivetrackMeta method to it; the registry enforces the constraint
// at compile time so a bare empty interface cannot leak into a
// Registry instantiation.
//
// The marker method body must be empty. Its purpose is purely to
// constrain the type parameter so that staticcheck-extra's no-any
// rule sees a non-empty interface constraint at every Registry,
// Session, Predicate, and Options use site.
type Meta interface {
	IsLivetrackMeta()
}
