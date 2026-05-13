// Package liveruntime defines the provider-neutral live-session runtime
// contract used between provider lifecycle packages and the daemon.
package liveruntime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/session"
)

// LiveRuntime is the provider-owned live-session control contract.
type LiveRuntime interface {
	Start(context.Context, StartRequest) (*LiveSession, error)
	Attach(context.Context, AttachRequest) (*LiveSession, error)
	Stop(context.Context, StopRequest) error
	Send(context.Context, SendRequest) (*LiveTurn, error)
	Stream(context.Context, StreamRequest) (<-chan LiveEvent, error)
}

// Deps contains daemon-provided ports used by provider runtimes.
type Deps struct {
	SessionStore     SessionStore
	LiveTrack        LiveTrack
	WorkerSupervisor WorkerSupervisor
	TranscriptTailer TranscriptTailer
	Logger           Logger
	Now              func() time.Time
}

// Factory constructs a provider live runtime from daemon-provided ports.
type Factory func(Deps) LiveRuntime

// StartRequest describes a provider live session start request.
type StartRequest struct {
	Provider    session.ProviderID
	SessionName string
	SessionID   string
	Basedir     string
	Model       string
	Effort      EffortLevel
	Incognito   bool
	PeerAddr    string
}

// AttachRequest describes an existing provider live session reattach request.
type AttachRequest struct {
	Provider    session.ProviderID
	SessionName string
	SessionID   string
	Basedir     string
	Model       string
	Status      string
	Effort      EffortLevel
	Incognito   bool
	Reason      string
}

// SendRequest describes one user message sent to a provider live session.
type SendRequest struct {
	Provider  session.ProviderID
	SessionID string
	Text      string
	Basedir   string
	Model     string
	Effort    EffortLevel
}

// StreamRequest identifies one provider live-session stream.
type StreamRequest struct {
	Provider    session.ProviderID
	SessionName string
	SessionID   string
	TurnID      string
}

// StopRequest identifies a provider live session or turn to stop.
type StopRequest struct {
	Provider    session.ProviderID
	SessionName string
	SessionID   string
	TurnID      string
	Reason      string
}

// LiveSession is the provider-neutral live-session handle.
type LiveSession struct {
	Provider       session.ProviderID
	SessionName    string
	SessionID      string
	Status         string
	Basedir        string
	URL            string
	StartedAt      time.Time
	SupportsSend   bool
	SupportsStream bool
	SupportsStop   bool
}

// LiveTurn is the provider-neutral result of starting or accepting one turn.
type LiveTurn struct {
	SessionID string
	TurnID    string
	Status    string
}

// LiveEvent is one provider-neutral live stream event.
type LiveEvent struct {
	SessionID string
	Kind      string
	Role      string
	Text      string
	Timestamp time.Time
	Err       error
}

// EffortLevel is the provider-neutral live-session effort string.
type EffortLevel string

// SessionStore is the session storage port used by provider runtimes.
type SessionStore interface {
	session.Store
}

// LiveMeta is the livetrack metadata shape for provider live sessions.
type LiveMeta struct {
	Provider      string
	LiveSessionID string
	WorkerPID     int
	Lease         string
}

var (
	_ livetrack.Meta = (*LiveMeta)(nil)
	_ LiveTrack      = (*livetrack.Registry[LiveMeta])(nil)
)

// IsLivetrackMeta marks LiveMeta as livetrack metadata.
func (LiveMeta) IsLivetrackMeta() {}

func init() {
	var meta LiveMeta
	meta.IsLivetrackMeta()
}

// LiveTrack is the livetrack registry port used by provider runtimes.
type LiveTrack interface {
	Register(context.Context, string, LiveMeta, livetrack.Closer, ...livetrack.RegisterOption) (*livetrack.Session[LiveMeta], error)
	Release(context.Context, *livetrack.Session[LiveMeta], string)
}

// Logger is the structured logging port used by provider runtimes.
type Logger interface {
	DebugContext(context.Context, string, ...slog.Attr)
	InfoContext(context.Context, string, ...slog.Attr)
	WarnContext(context.Context, string, ...slog.Attr)
	ErrorContext(context.Context, string, ...slog.Attr)
}

// TranscriptLine is one provider-neutral transcript tail line.
type TranscriptLine struct {
	Role           string
	Text           string
	TimestampNanos int64
}

// TranscriptTailer is the transcript subscription port used by provider runtimes.
type TranscriptTailer interface {
	Subscribe(context.Context, string, string, int64) (<-chan TranscriptLine, func(), error)
}

// WorkerStartRequest describes a provider worker process launch.
type WorkerStartRequest struct {
	SessionName string
	SessionID   string
	Basedir     string
	Incognito   bool
}

// WorkerHandle is an opaque process handle owned by a worker supervisor.
type WorkerHandle struct {
	PID  int
	Done <-chan struct{}
}

// WorkerSupervisor is the provider-runtime port for worker process control.
type WorkerSupervisor interface {
	Start(context.Context, WorkerStartRequest) (*WorkerHandle, error)
	Interrupt(context.Context, *WorkerHandle, time.Duration) error
	Send(context.Context, string, []byte) (int, error)
	Reachable(string) bool
}

var registry = struct {
	mu        sync.RWMutex
	factories map[session.ProviderID]Factory
}{
	mu:        sync.RWMutex{},
	factories: make(map[session.ProviderID]Factory),
}

// Register adds a live runtime factory for a provider.
var Register = func(provider session.ProviderID, factory Factory) {
	normalized := session.NormalizeProviderID(provider)
	if factory == nil {
		slog.Error("liveruntime.registry.nil_factory",
			"component", "providers",
			"provider", normalized,
			"err", fmt.Errorf("nil liveruntime factory for provider %q", normalized),
		)
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factories[normalized] != nil {
		slog.Error("liveruntime.registry.duplicate_factory",
			"component", "providers",
			"provider", normalized,
			"err", fmt.Errorf("duplicate liveruntime factory for provider %q", normalized),
		)
		return
	}
	registry.factories[normalized] = factory
}

// Get returns a provider live runtime for the requested provider.
var Get = func(provider session.ProviderID, deps Deps) (LiveRuntime, bool) {
	normalized := session.NormalizeProviderID(provider)
	registry.mu.RLock()
	factory := registry.factories[normalized]
	registry.mu.RUnlock()
	if factory == nil {
		return nil, false
	}
	return factory(deps), true
}
