package hook

import (
	"fmt"
	"os"
	"slices"

	"goodkind.io/clyde/internal/session"
)

func resolveSessionName(hookData SessionStartInput, store session.Store, fullFallback bool) (string, error) {
	if name := os.Getenv("CLYDE_SESSION_NAME"); name != "" {
		return name, nil
	}

	if !fullFallback {
		return "", nil
	}

	if name := readLastEnvFileValue("CLYDE_SESSION"); name != "" {
		return name, nil
	}

	return findSessionByUUID(store, hookData.SessionID)
}

func findSessionByUUID(store session.Store, uuid string) (string, error) {
	sess, err := findSessionByUUIDSession(store, uuid)
	if err != nil {
		return "", err
	}
	return sess.Name, nil
}

func findSessionByUUIDSession(store session.Store, uuid string) (*session.Session, error) {
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

	hookLog.Warn("hook.resolve_session.uuid_not_found",
		"component", "hook",
		"subcomponent", "resolve",
		"session_id", uuid,
	)
	return nil, fmt.Errorf("no session found with UUID %s", uuid)
}
