package session

import (
	"slices"
	"strings"
)

// ProviderSessionID is a provider-scoped session identifier.
type ProviderSessionID struct {
	Provider ProviderID `json:"provider"`
	ID       string     `json:"id"`
}

// Normalized returns a trimmed provider session id with default provider fallback.
func (id ProviderSessionID) Normalized() ProviderSessionID {
	return id.NormalizedForProvider(defaultProviderInfo.ID)
}

// NormalizedForProvider returns a trimmed provider session id, using fallback
// when the id does not carry its own provider namespace.
func (id ProviderSessionID) NormalizedForProvider(fallback ProviderID) ProviderSessionID {
	provider := ProviderID(strings.TrimSpace(string(id.Provider)))
	if provider == ProviderUnknown {
		provider = NormalizeProviderID(fallback)
	}
	return ProviderSessionID{
		Provider: provider,
		ID:       strings.TrimSpace(id.ID),
	}
}

// IsZero reports whether the id does not point at a concrete provider session.
func (id ProviderSessionID) IsZero() bool {
	return strings.TrimSpace(id.ID) == ""
}

// Key returns the stable logical identity key for maps and dedupe sets.
func (id ProviderSessionID) Key() string {
	normalized := id.Normalized()
	if normalized.IsZero() {
		return ""
	}
	return "provider:" + string(normalized.Provider) + ":sid:" + normalized.ID
}

// SessionIdentity holds the current and historical provider session ids.
type SessionIdentity struct {
	Name     string
	Current  ProviderSessionID
	Previous []ProviderSessionID
}

// HasID reports whether the current or historical identity matches the raw id.
func (identity SessionIdentity) HasID(rawID string) bool {
	trimmed := strings.TrimSpace(rawID)
	if trimmed == "" {
		return false
	}
	if identity.Current.Normalized().ID == trimmed {
		return true
	}
	for _, previous := range identity.Previous {
		if previous.Normalized().ID == trimmed {
			return true
		}
	}
	return false
}

// CurrentIdentity returns the session's current provider-scoped identity.
func CurrentIdentity(sess *Session) (ProviderSessionID, bool) {
	if sess == nil {
		return ProviderSessionID{}, false
	}
	current := sess.Identity().Current.Normalized()
	if current.IsZero() {
		return ProviderSessionID{}, false
	}
	return current, true
}

// HistoricalIdentities returns every exact provider-scoped identifier that
// should resolve back to the same logical session.
func HistoricalIdentities(sess *Session) []ProviderSessionID {
	if sess == nil {
		return nil
	}
	identity := sess.Identity()
	out := make([]ProviderSessionID, 0, 1+len(identity.Previous))
	if current := identity.Current.Normalized(); !current.IsZero() {
		out = append(out, current)
	}
	for _, previous := range identity.Previous {
		normalized := previous.Normalized()
		if normalized.IsZero() {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

// MatchesAnySessionID reports whether query matches any current or historical
// direct session identifier for sess, regardless of provider namespace.
func MatchesAnySessionID(sess *Session, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	for _, identity := range HistoricalIdentities(sess) {
		if identity.ID == query {
			return true
		}
	}
	return false
}

// IdentityKey returns the logical identity used to collapse stale aliases that
// point at the same underlying provider session.
func IdentityKey(sess *Session) string {
	if sess == nil {
		return ""
	}
	if key := sess.Identity().Current.Key(); key != "" {
		return key
	}
	return "name:" + sess.Name
}

// PreferIdentityWinner reports whether candidate should replace existing when
// two session rows describe the same logical session.
func PreferIdentityWinner(candidate, existing *Session) bool {
	if candidate == nil {
		return false
	}
	if existing == nil {
		return true
	}
	candidateAuto := LooksLikeAutoAdoptedName(candidate.Name)
	existingAuto := LooksLikeAutoAdoptedName(existing.Name)
	if candidateAuto != existingAuto {
		return !candidateAuto
	}
	if candidate.Metadata.Title != "" && existing.Metadata.Title == "" {
		return true
	}
	if existing.Metadata.Title != "" && candidate.Metadata.Title == "" {
		return false
	}
	if candidate.Metadata.LastAccessed.After(existing.Metadata.LastAccessed) {
		return true
	}
	if existing.Metadata.LastAccessed.After(candidate.Metadata.LastAccessed) {
		return false
	}
	if candidate.Metadata.Created.After(existing.Metadata.Created) {
		return true
	}
	if existing.Metadata.Created.After(candidate.Metadata.Created) {
		return false
	}
	return candidate.Name < existing.Name
}

// LooksLikeAutoAdoptedName matches the common workspace-plus-short-id names
// produced by background adoption before a user assigns a friendlier title.
func LooksLikeAutoAdoptedName(name string) bool {
	if len(name) <= 9 {
		return false
	}
	cut := strings.LastIndexByte(name, '-')
	if cut <= 0 || cut >= len(name)-1 {
		return false
	}
	suffix := name[cut+1:]
	if len(suffix) != 8 {
		return false
	}
	for _, ch := range suffix {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func appendUniqueString(existing []string, value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return existing
	}
	if slices.Contains(existing, trimmed) {
		return existing
	}
	return append(existing, trimmed)
}

func appendProviderSessionID(existing []ProviderSessionID, value ProviderSessionID) []ProviderSessionID {
	normalized := value.Normalized()
	if normalized.IsZero() {
		return existing
	}
	for _, item := range existing {
		if item.Normalized() == normalized {
			return existing
		}
	}
	return append(existing, normalized)
}
