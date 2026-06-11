package livetrack

import (
	"context"
	"time"
)

// drainMember is the type-erased view of a registry the Group drives. Each
// Registry[M] erases to this via registryMember so the group can hold members
// of different Meta types in one slice.
type drainMember interface {
	component() string
	activeCount(grace time.Duration) int
	drainWith(ctx context.Context, reason string, opts DrainOptions) DrainResult
	spec() MemberSpec
}

// registryMember erases a Registry[M] to drainMember. It is constructed by
// Attach and stored on the Group; callers never build it directly.
type registryMember[M Meta] struct {
	reg     *Registry[M]
	memSpec MemberSpec
}

func (m registryMember[M]) component() string { return m.reg.component }

func (m registryMember[M]) activeCount(grace time.Duration) int {
	return m.reg.ActiveCount(grace)
}

func (m registryMember[M]) drainWith(ctx context.Context, reason string, opts DrainOptions) DrainResult {
	return m.reg.drainWith(ctx, reason, opts)
}

func (m registryMember[M]) spec() MemberSpec { return m.memSpec }
