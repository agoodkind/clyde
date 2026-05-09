package hook

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/session"
)

func resolveSessionName(hookData SessionStartInput, store session.Store, fullFallback bool) (string, error) {
	if sess := resolveSessionFromIdentifiers(hookData, store); sess != nil {
		return sess.Name, nil
	}

	if name := strings.TrimSpace(os.Getenv(envSessionName)); name != "" {
		return name, nil
	}

	if !fullFallback {
		return "", nil
	}

	if name := strings.TrimSpace(readLastEnvFileValue(envLegacySessionName)); name != "" {
		return name, nil
	}

	return findSessionByUUID(store, hookData.SessionID)
}

func resolveSessionFromIdentifiers(hookData SessionStartInput, store session.Store) *session.Session {
	for _, sessionID := range orderedSessionIDCandidates(hookData) {
		sess, err := findExistingSessionByUUIDSession(store, sessionID)
		if err != nil {
			hookLog.Warn("hook.resolve_session.identifier_lookup_failed",
				"component", "hook",
				"subcomponent", "resolve",
				"session_id", sessionID,
				"err", err,
			)
			continue
		}
		if sess != nil {
			return sess
		}
	}
	return nil
}

func orderedSessionIDCandidates(hookData SessionStartInput) []string {
	envValue := os.Getenv(envSessionID)
	values := []string{
		hookData.SessionID,
		envValue,
	}
	if hookData.Source == "compact" || hookData.Source == "clear" {
		values = []string{
			envValue,
			hookData.SessionID,
		}
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func resultSessionName(hookData SessionStartInput, store session.Store) string {
	if sess := resolveSessionFromIdentifiers(hookData, store); sess != nil {
		return sess.Name
	}
	return strings.TrimSpace(os.Getenv(envSessionName))
}

func findSessionByUUID(store session.Store, uuid string) (string, error) {
	sess, err := findSessionByUUIDSession(store, uuid)
	if err != nil {
		return "", err
	}
	return sess.Name, nil
}

func findSessionByUUIDSession(store session.Store, uuid string) (*session.Session, error) {
	sess, err := findExistingSessionByUUIDSession(store, uuid)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess, nil
	}

	hookLog.Warn("hook.resolve_session.uuid_not_found",
		"component", "hook",
		"subcomponent", "resolve",
		"session_id", uuid,
	)
	return nil, fmt.Errorf("no session found with UUID %s", uuid)
}

func findExistingSessionByUUIDSession(store session.Store, uuid string) (*session.Session, error) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil, nil
	}
	sessions, err := store.List()
	if err != nil {
		hookLog.Warn("hook.resolve_session.list_failed",
			"component", "hook",
			"subcomponent", "resolve",
			"err", err,
		)
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	for _, sess := range sessions {
		if sess.Metadata.ProviderSessionID() == uuid {
			return sess, nil
		}
	}

	for _, sess := range sessions {
		if slices.Contains(sess.Metadata.PreviousProviderSessionIDStrings(), uuid) {
			return sess, nil
		}
	}

	return nil, nil
}
