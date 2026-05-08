package session

import "time"

// Session represents a named provider session.
type Session struct {
	Name     string
	Metadata Metadata
}

// Metadata represents the session metadata stored in metadata.json.
type Metadata struct {
	Name                 string                 `json:"name"`
	Provider             ProviderID             `json:"provider,omitempty"`
	SessionID            string                 `json:"sessionId"`
	TranscriptPath       string                 `json:"transcriptPath,omitempty"`
	ProviderState        *ProviderOwnedMetadata `json:"providerState,omitempty"`
	WorkDir              string                 `json:"workDir,omitempty"`
	Created              time.Time              `json:"created"`
	LastAccessed         time.Time              `json:"lastAccessed"`
	ParentSession        string                 `json:"parentSession,omitempty"`
	IsForkedSession      bool                   `json:"isForkedSession"`
	IsIncognito          bool                   `json:"isIncognito"`
	PreviousSessionIDs   []string               `json:"previousSessionIds,omitempty"`
	Context              string                 `json:"context,omitempty"`
	HasCustomOutputStyle bool                   `json:"hasCustomOutputStyle,omitempty"`
	WorkspaceRoot        string                 `json:"workspaceRoot,omitempty"`

	// ContextMessageCount is the message count at the moment Context was
	// last generated. The TUI uses it to decide when the summary is stale
	// and should be regenerated in the background.
	ContextMessageCount int `json:"contextMessageCount,omitempty"`

	// DisplayTitle preserves the original user-given chat name that
	// Claude Code stores in transcript "custom-title" entries. It is the
	// human-readable form surfaced in the TUI. The session Name is a
	// sanitized derivative used as the directory identifier and is what
	// clyde resume, compact, and other verbs accept. DisplayTitle stays
	// in sync with the latest custom-title entry seen during scan; the
	// Name never renames post-adoption because that would break
	// previousSessionIds and parentSession references.
	DisplayTitle string `json:"displayTitle,omitempty"`

	// AutoNameState records the session's position in the auto-rename
	// state machine. The zero value is untouched, which matches sessions
	// written before this field existed.
	AutoNameState AutoNameState `json:"autoNameState,omitempty"`

	// AutoNameSource records which subsystem produced the current name.
	// A zero value means the daemon has not yet attributed a source.
	AutoNameSource AutoNameSource `json:"autoNameSource,omitempty"`

	// LastAutoNameAt records the most recent auto-rename attempt for the
	// session. The zero value means no auto-rename has run.
	LastAutoNameAt time.Time `json:"lastAutoNameAt,omitempty"`
}

// ProviderOwnedMetadata is the provider-neutral persisted identity and artifact
// section for a session. Legacy top-level fields are still mirrored for
// backward compatibility with existing metadata.json files.
type ProviderOwnedMetadata struct {
	Current   ProviderSessionID   `json:"current"`
	Previous  []ProviderSessionID `json:"previous,omitempty"`
	Artifacts ProviderArtifacts   `json:"artifacts,omitzero"`
}

// ProviderArtifacts records provider-owned paths and handles needed by session
// features. TranscriptPath is currently populated by Claude; future providers
// can fill the artifacts that map to their own storage model.
type ProviderArtifacts struct {
	TranscriptPath string `json:"transcriptPath,omitempty"`
}

// Settings represents Claude Code session-specific settings stored in settings.json.
type Settings struct {
	Model         string      `json:"model,omitempty"`
	EffortLevel   string      `json:"effortLevel,omitempty"`
	OutputStyle   string      `json:"outputStyle,omitempty"`
	RemoteControl bool        `json:"remoteControl,omitempty"`
	ContextWindow string      `json:"contextWindow,omitempty"`
	Permissions   Permissions `json:"permissions,omitzero"`
}

// Permissions represents the permissions configuration for a session.
type Permissions struct {
	Allow                        []string `json:"allow,omitempty"`
	Ask                          []string `json:"ask,omitempty"`
	Deny                         []string `json:"deny,omitempty"`
	AdditionalDirectories        []string `json:"additionalDirectories,omitempty"`
	DefaultMode                  string   `json:"defaultMode,omitempty"`
	DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode,omitempty"`
}

// NewSession creates a new default-provider session with the given name and
// provider session id.
func NewSession(name, sessionID string) *Session {
	now := currentTime()
	sess := &Session{
		Name: name,
		Metadata: Metadata{
			Name:            name,
			Provider:        ProviderClaude,
			SessionID:       sessionID,
			Created:         now,
			LastAccessed:    now,
			IsForkedSession: false,
		},
	}
	sess.Metadata.NormalizeProviderState()
	return sess
}

// UpdateLastAccessed updates the lastAccessed timestamp to now.
func (s *Session) UpdateLastAccessed() {
	s.Metadata.LastAccessed = currentTime()
}

// ProviderID returns the normalized provider for the session.
func (s *Session) ProviderID() ProviderID {
	return s.Metadata.ProviderID()
}

// Identity returns the provider-aware identity for the session.
func (s *Session) Identity() SessionIdentity {
	return s.Metadata.Identity(s.Name)
}

// SessionProviderCapabilities returns the capabilities for the session provider.
func (s *Session) SessionProviderCapabilities() ProviderCapabilities {
	return ProviderInfo(s.ProviderID()).Capabilities
}

// ProviderID returns the normalized provider for stored metadata.
func (m Metadata) ProviderID() ProviderID {
	return NormalizeProviderID(m.Provider)
}

// ProviderSessionID returns the current provider-scoped session identifier.
func (m Metadata) ProviderSessionID() string {
	return m.Identity("").Current.Normalized().ID
}

// ProviderTranscriptPath returns the primary provider transcript/log artifact
// path for the session, with legacy metadata fallback.
func (m Metadata) ProviderTranscriptPath() string {
	if m.ProviderState != nil && m.ProviderState.Artifacts.TranscriptPath != "" {
		return m.ProviderState.Artifacts.TranscriptPath
	}
	return m.TranscriptPath
}

// PreviousProviderSessionIDs returns historical provider-scoped identities.
func (m Metadata) PreviousProviderSessionIDs() []ProviderSessionID {
	return m.Identity("").Previous
}

// PreviousProviderSessionIDStrings returns historical provider IDs in the
// current provider namespace for legacy callers that still speak raw IDs.
func (m Metadata) PreviousProviderSessionIDStrings() []string {
	previous := m.PreviousProviderSessionIDs()
	out := make([]string, 0, len(previous))
	for _, id := range previous {
		normalized := id.Normalized()
		if !normalized.IsZero() {
			out = append(out, normalized.ID)
		}
	}
	return out
}

// Identity returns the provider-aware identity for stored metadata.
func (m Metadata) Identity(name string) SessionIdentity {
	provider := m.ProviderID()
	if m.ProviderState != nil {
		current := m.ProviderState.Current.NormalizedForProvider(provider)
		previous := make([]ProviderSessionID, 0, len(m.ProviderState.Previous))
		for _, previousID := range m.ProviderState.Previous {
			normalized := previousID.NormalizedForProvider(provider)
			if !normalized.IsZero() {
				previous = append(previous, normalized)
			}
		}
		return SessionIdentity{
			Name:     name,
			Current:  current,
			Previous: previous,
		}
	}
	previous := make([]ProviderSessionID, 0, len(m.PreviousSessionIDs))
	for _, previousSessionID := range m.PreviousSessionIDs {
		previous = append(previous, ProviderSessionID{
			Provider: provider,
			ID:       previousSessionID,
		})
	}
	return SessionIdentity{
		Name: name,
		Current: ProviderSessionID{
			Provider: provider,
			ID:       m.SessionID,
		},
		Previous: previous,
	}
}

// NormalizeProviderState fills the provider-owned metadata section from legacy
// fields and mirrors it back to legacy fields for older Clyde binaries.
func (m *Metadata) NormalizeProviderState() {
	if m == nil {
		return
	}
	provider := m.ProviderID()
	if m.ProviderState == nil {
		m.ProviderState = &ProviderOwnedMetadata{}
	}
	current := m.ProviderState.Current.NormalizedForProvider(provider)
	if current.IsZero() && m.SessionID != "" {
		current = ProviderSessionID{Provider: provider, ID: m.SessionID}.NormalizedForProvider(provider)
	}
	m.ProviderState.Current = current

	if len(m.ProviderState.Previous) == 0 && len(m.PreviousSessionIDs) > 0 {
		m.ProviderState.Previous = make([]ProviderSessionID, 0, len(m.PreviousSessionIDs))
		for _, previousSessionID := range m.PreviousSessionIDs {
			id := ProviderSessionID{Provider: provider, ID: previousSessionID}.NormalizedForProvider(provider)
			if !id.IsZero() {
				m.ProviderState.Previous = append(m.ProviderState.Previous, id)
			}
		}
	}
	for i := range m.ProviderState.Previous {
		normalized := m.ProviderState.Previous[i].NormalizedForProvider(provider)
		m.ProviderState.Previous[i] = normalized
	}
	if m.ProviderState.Artifacts.TranscriptPath == "" && m.TranscriptPath != "" {
		m.ProviderState.Artifacts.TranscriptPath = m.TranscriptPath
	}

	m.Provider = current.Provider
	m.SessionID = current.ID
	m.PreviousSessionIDs = m.PreviousSessionIDs[:0]
	for _, previous := range m.ProviderState.Previous {
		if !previous.Normalized().IsZero() {
			m.PreviousSessionIDs = appendUniqueString(m.PreviousSessionIDs, previous.Normalized().ID)
		}
	}
	m.TranscriptPath = m.ProviderState.Artifacts.TranscriptPath
}

// SetProviderTranscriptPath updates the provider-owned primary transcript/log
// artifact and mirrors the legacy field.
func (m *Metadata) SetProviderTranscriptPath(path string) {
	if m == nil {
		return
	}
	m.NormalizeProviderState()
	m.ProviderState.Artifacts.TranscriptPath = path
	m.TranscriptPath = path
}

// RotateIdentity records the current provider session id as historical state and
// replaces it with the new primary identity.
func (s *Session) RotateIdentity(next ProviderSessionID) {
	identity := s.Identity()
	next = next.NormalizedForProvider(s.ProviderID())
	if current := identity.Current.Normalized(); !current.IsZero() && current != next {
		s.Metadata.PreviousSessionIDs = appendUniqueString(s.Metadata.PreviousSessionIDs, current.ID)
		s.Metadata.NormalizeProviderState()
		s.Metadata.ProviderState.Previous = appendProviderSessionID(s.Metadata.ProviderState.Previous, current)
	}
	s.Metadata.Provider = next.Provider
	s.Metadata.SessionID = next.ID
	s.Metadata.NormalizeProviderState()
	s.Metadata.ProviderState.Current = next
	s.Metadata.Provider = next.Provider
	s.Metadata.SessionID = next.ID
}

// AddPreviousSessionID appends the current session ID to the history and updates to the new ID.
// This is idempotent - won't add duplicates.
func (s *Session) AddPreviousSessionID(newSessionID string) {
	s.RotateIdentity(ProviderSessionID{
		Provider: s.ProviderID(),
		ID:       newSessionID,
	})
}
