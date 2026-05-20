package session

import (
	"context"
	"strings"
)

// ProviderID identifies the upstream that owns a session id namespace.
type ProviderID string

const (
	ProviderUnknown ProviderID = ""
	ProviderClaude  ProviderID = "claude"
	ProviderCodex   ProviderID = "codex"
)

// ProviderCapabilities describes the session lifecycle features a provider supports.
type ProviderCapabilities struct {
	ResumeByID          bool
	ForkByID            bool
	SessionIDRotation   bool
	CustomTitles        bool
	PerSessionSettings  bool
	HistoryRead         bool
	HistoryExport       bool
	LiveTail            bool
	LiveInput           bool
	RemoteControl       bool
	TranscriptTail      bool
	TranscriptExport    bool
	Compaction          bool
	ProviderArtifactGC  bool
	ContextUsageInspect bool
}

// ProviderInfoRecord is a typed provider descriptor used by session metadata.
type ProviderInfoRecord struct {
	ID           ProviderID
	DisplayName  string
	Capabilities ProviderCapabilities
}

// SessionHistoryBoundary describes the provider-neutral read side of a session.
// Legacy Claude transcript paths are represented as the primary artifact while
// newer providers can map their own durable history handle to the same field.
type SessionHistoryBoundary struct {
	Provider           ProviderID
	CurrentSessionID   string
	PreviousSessionIDs []string
	PrimaryArtifact    string
	Readable           bool
	Exportable         bool
}

// LiveSessionBoundary describes the provider-neutral live runtime side of a
// session, including whether callers may tail output or send input.
type LiveSessionBoundary struct {
	Provider        ProviderID
	SessionID       string
	TailReadable    bool
	InputWritable   bool
	RemoteControlOn bool
	BridgeURL       string
	BridgeSessionID string
}

// ProviderRuntimeBoundary is the combined provider-neutral contract exposed to
// daemon clients that need history and live-session capabilities.
type ProviderRuntimeBoundary struct {
	History SessionHistoryBoundary
	Live    LiveSessionBoundary
}

// ProviderRuntimeBoundary returns the provider-neutral history/live runtime
// contract for this session. Compatibility fields such as TranscriptExport and
// RemoteControl still feed the generic flags for existing Claude rows.
func (s *Session) ProviderRuntimeBoundary() ProviderRuntimeBoundary {
	if s == nil {
		return ProviderRuntimeBoundary{}
	}
	caps := s.SessionProviderCapabilities()
	return ProviderRuntimeBoundary{
		History: SessionHistoryBoundary{
			Provider:           s.ProviderID(),
			CurrentSessionID:   s.Metadata.ProviderSessionID(),
			PreviousSessionIDs: append([]string(nil), s.Metadata.PreviousProviderSessionIDStrings()...),
			PrimaryArtifact:    s.Metadata.ProviderTranscriptPath(),
			Readable:           caps.HistoryRead || caps.TranscriptTail || caps.TranscriptExport,
			Exportable:         caps.HistoryExport || caps.TranscriptExport,
		},
		Live: LiveSessionBoundary{
			Provider:        s.ProviderID(),
			SessionID:       s.Metadata.ProviderSessionID(),
			TailReadable:    caps.LiveTail || caps.TranscriptTail,
			InputWritable:   caps.LiveInput || caps.RemoteControl,
			RemoteControlOn: false,
		},
	}
}

var defaultProviderInfo = ProviderInfoRecord{
	ID:          ProviderClaude,
	DisplayName: "Claude",
	Capabilities: ProviderCapabilities{
		ResumeByID:          true,
		ForkByID:            true,
		SessionIDRotation:   true,
		CustomTitles:        true,
		PerSessionSettings:  true,
		HistoryRead:         true,
		HistoryExport:       true,
		LiveTail:            true,
		LiveInput:           true,
		RemoteControl:       true,
		TranscriptTail:      true,
		TranscriptExport:    true,
		Compaction:          true,
		ProviderArtifactGC:  true,
		ContextUsageInspect: true,
	},
}

var codexProviderInfo = ProviderInfoRecord{
	ID:          ProviderCodex,
	DisplayName: "Codex",
	Capabilities: ProviderCapabilities{
		ResumeByID:    true,
		HistoryRead:   true,
		HistoryExport: true,
		LiveTail:      true,
		LiveInput:     true,
	},
}

// SessionProvider exposes the provider assigned to a session row.
type SessionProvider interface {
	ProviderID() ProviderID
}

// CapabilityProvider exposes provider capability metadata to callers that need
// provider-aware branching without knowing a concrete implementation.
type CapabilityProvider interface {
	SessionProviderCapabilities() ProviderCapabilities
}

// LaunchIntent describes the generic session action the caller wants a provider
// lifecycle implementation to perform.
type LaunchIntent string

const (
	LaunchIntentNewSession LaunchIntent = "new_session"
)

// LaunchOptions carries provider-neutral launch intent from cmd into the
// provider-owned lifecycle implementation.
type LaunchOptions struct {
	WorkDir             string
	Intent              LaunchIntent
	EnableRemoteControl bool
}

// StartRequest is the generic start-session request above provider code.
type StartRequest struct {
	SessionName string
	// SessionID is an optional provider session id the daemon already minted and
	// persisted. When set, the provider launches the tool with this id instead of
	// minting its own. Empty for providers that do not pre-assign ids.
	SessionID string
	Launch    LaunchOptions
}

// ResumeOptions carries provider-neutral resume intent from cmd into the
// provider-owned lifecycle implementation.
type ResumeOptions struct {
	CurrentWorkDir   string
	EnableSelfReload bool
}

// ResumeRequest is the generic resume-session request above provider code.
type ResumeRequest struct {
	Session *Session
	Options ResumeOptions
}

// OpaqueResumeRequest carries a provider-native resume query when clyde does
// not have a registered session row for the target yet.
type OpaqueResumeRequest struct {
	Query          string
	AdditionalArgs []string
}

// ContextMessage is provider-neutral recent conversation text used for generic
// context refresh flows.
type ContextMessage struct {
	Role string
	Text string
}

// DeleteArtifactsRequest carries the provider-owned artifact cleanup request for
// a logical session row.
type DeleteArtifactsRequest struct {
	Session   *Session
	ClydeRoot string
}

// DeletedArtifacts summarizes provider-owned artifacts removed for a session.
type DeletedArtifacts struct {
	Transcripts []string
	AgentLogs   []string
}

// SessionLauncher is the narrow lifecycle contract cmd should use for new
// interactive session startup.
type SessionLauncher interface {
	StartInteractive(ctx context.Context, req StartRequest) error
}

// SessionResumer is the narrow lifecycle contract cmd should use for resuming
// an already-registered interactive session.
type SessionResumer interface {
	ResumeInteractive(ctx context.Context, req ResumeRequest) error
}

// OpaqueSessionResumer is the narrow lifecycle contract cmd should use when it
// needs the provider to resolve an unmanaged upstream session reference.
type OpaqueSessionResumer interface {
	ResumeOpaqueInteractive(ctx context.Context, req OpaqueResumeRequest) error
}

// ResumeInstructionProvider exposes provider-native resume hints without making
// cmd know the exact upstream CLI or session-id shape.
type ResumeInstructionProvider interface {
	ResumeInstructions(sess *Session) []string
}

// ContextMessageProvider exposes provider-owned transcript shaping so generic
// layers do not parse provider transcript files directly.
type ContextMessageProvider interface {
	RecentContextMessages(sess *Session, limit, maxLen int) []ContextMessage
}

// CurrentTitleProvider lets providers expose their current live title without
// making generic session code parse provider-owned transcript shapes.
type CurrentTitleProvider interface {
	// TODO: Design provider title unification by writing a custom-title event on clyde rename and guarding provider auto-title re-emission.
	ReadCurrentTitle(transcriptPath string) (string, error)
}

// NameProvider lets providers own how exact display names are observed and
// mutated while generic callers speak only in terms of logical sessions.
type NameProvider interface {
	GetSessionName(ctx context.Context, sess *Session) (string, error)
	RenameSession(ctx context.Context, sess *Session, newName string) error
}

// ArtifactCleaner deletes provider-owned files associated with a session row.
type ArtifactCleaner interface {
	DeleteArtifacts(ctx context.Context, req DeleteArtifactsRequest) (*DeletedArtifacts, error)
}

// NormalizeProviderID resolves empty legacy metadata to the current default provider.
func NormalizeProviderID(provider ProviderID) ProviderID {
	trimmed := ProviderID(strings.TrimSpace(string(provider)))
	if trimmed == ProviderUnknown {
		return defaultProviderInfo.ID
	}
	return trimmed
}

// ProviderInfo returns the typed descriptor for a provider id.
func ProviderInfo(provider ProviderID) ProviderInfoRecord {
	normalized := NormalizeProviderID(provider)
	if normalized == defaultProviderInfo.ID {
		return defaultProviderInfo
	}
	if normalized == codexProviderInfo.ID {
		return codexProviderInfo
	}
	var emptyCapabilities ProviderCapabilities
	return ProviderInfoRecord{
		ID:           normalized,
		DisplayName:  "",
		Capabilities: emptyCapabilities,
	}
}

// ResumeInstructions returns provider-native resume hints for a concrete
// session without making generic callers know the upstream CLI shape.
func ResumeInstructions(sess *Session) []string {
	if sess == nil {
		return nil
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return nil
	}
	switch NormalizeProviderID(sess.ProviderID()) {
	case ProviderUnknown:
		return nil
	case ProviderCodex:
		return []string{"codex resume " + sessionID}
	case ProviderClaude:
		return []string{"claude --resume " + sessionID}
	default:
		return nil
	}
}
