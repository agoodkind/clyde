package session

import (
	"strings"
	"sync"
)

// CurrentTitleResolver reads the provider-owned live title for one transcript.
type CurrentTitleResolver func(provider ProviderID, transcriptPath string) (string, error)

var currentTitleResolverState = struct {
	sync.RWMutex
	resolver CurrentTitleResolver
}{
	RWMutex:  sync.RWMutex{},
	resolver: nil,
}

// RegisterCurrentTitleResolver installs the provider-backed title resolver.
func RegisterCurrentTitleResolver(resolver CurrentTitleResolver) {
	currentTitleResolverState.Lock()
	defer currentTitleResolverState.Unlock()
	currentTitleResolverState.resolver = resolver
}

// ProviderTitle returns the current provider-owned live title for the session.
func (s *Session) ProviderTitle() string {
	if s == nil {
		return ""
	}
	transcriptPath := strings.TrimSpace(s.Metadata.ProviderTranscriptPath())
	if transcriptPath == "" {
		sessionLog.Debug("session.provider_title.missing_transcript",
			"component", "session",
			"subcomponent", "provider_title",
			"session", s.Name,
			"provider", s.ProviderID(),
		)
		return ""
	}
	resolver := currentTitleResolver()
	if resolver == nil {
		sessionLog.Debug("session.provider_title.missing_resolver",
			"component", "session",
			"subcomponent", "provider_title",
			"session", s.Name,
			"provider", s.ProviderID(),
		)
		return ""
	}
	title, err := resolver(s.ProviderID(), transcriptPath)
	if err != nil {
		sessionLog.Debug("session.provider_title.read_failed",
			"component", "session",
			"subcomponent", "provider_title",
			"session", s.Name,
			"provider", s.ProviderID(),
			"transcript_path", transcriptPath,
			"err", err,
		)
		return ""
	}
	return strings.TrimSpace(title)
}

func currentTitleResolver() CurrentTitleResolver {
	currentTitleResolverState.RLock()
	defer currentTitleResolverState.RUnlock()
	return currentTitleResolverState.resolver
}
