// Package registry resolves provider-specific session runtimes.
package registry

import (
	"fmt"

	claudelifecycle "goodkind.io/clyde/internal/providers/claude/lifecycle"
	codexlifecycle "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/session"
)

// Runtime groups the provider-owned session behaviors that generic callers need.
type Runtime interface {
	session.SessionLauncher
	session.SessionResumer
	session.OpaqueSessionResumer
	session.ResumeInstructionProvider
	session.ContextMessageProvider
	session.NameProvider
	session.ArtifactCleaner
}

// Default returns the default provider runtime for session flows that have not
// resolved a stored session row yet.
func Default(store session.Store) (Runtime, error) {
	return ForProvider(session.ProviderClaude, store)
}

// ForSession returns the provider runtime for an adopted or registered session.
func ForSession(sess *session.Session, store session.Store) (Runtime, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	return ForProvider(sess.ProviderID(), store)
}

// ForProvider returns the provider runtime for the given provider id.
func ForProvider(provider session.ProviderID, store session.Store) (Runtime, error) {
	switch session.NormalizeProviderID(provider) {
	case session.ProviderClaude:
		if store == nil {
			return claudelifecycle.NewLifecycle(nil), nil
		}
		return claudelifecycle.NewLifecycle(store), nil
	case session.ProviderCodex:
		return codexlifecycle.NewLifecycle(), nil
	default:
		return nil, fmt.Errorf("unsupported session provider %q", session.NormalizeProviderID(provider))
	}
}
