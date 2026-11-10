package codex

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/config"
)

// LoadInstallationID returns the stable installation id used for the
// `x-codex-installation-id` header. The CLI and Desktop both read this
// from `~/.codex/installation_id`. We follow the same convention when
// the file exists. Otherwise we generate a clyde-owned uuid and persist
// it at `~/.config/clyde/codex-installation-id` so subsequent calls
// return the same value.
//
// The function caches the resolved id in process. The first call may
// touch disk; subsequent calls do not.
func LoadInstallationID() (string, error) {
	return defaultInstallationLoader.Load()
}

var defaultInstallationLoader = newInstallationLoader(installationFileFinder{})

type installationLoader struct {
	finder installationPathFinder
	mu     sync.Mutex
	cached string
}

type installationPathFinder interface {
	codexPath() (string, error)
	clydePath() (string, error)
}

type installationFileFinder struct{}

func (installationFileFinder) codexPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("adapter.codex.installation.user_home_failed", "concern", "adapter.providers.codex.request", "err", err)
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "installation_id"), nil
}

func (installationFileFinder) clydePath() (string, error) {
	return filepath.Join(filepath.Dir(config.GlobalConfigPath()), "codex-installation-id"), nil
}

func newInstallationLoader(finder installationPathFinder) *installationLoader {
	return &installationLoader{
		finder: finder, mu: sync.
			Mutex{},

		cached: "",
	}
}

func (l *installationLoader) Load() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cached != "" {
		return l.cached, nil
	}
	codexPath, err := l.finder.codexPath()
	if err == nil {
		if id, ok := readNonEmpty(codexPath); ok {
			l.cached = id
			return id, nil
		}
	}
	clydePath, err := l.finder.clydePath()
	if err != nil {
		return "", err
	}
	if id, ok := readNonEmpty(clydePath); ok {
		l.cached = id
		return id, nil
	}
	id, err := generateInstallationID()
	if err != nil {
		return "", err
	}
	if err := persistInstallationID(clydePath, id); err != nil {
		return "", err
	}
	l.cached = id
	return id, nil
}

func readNonEmpty(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", false
	}
	return id, true
}

// generateInstallationID returns a 32-character hex string. We pick hex
// over the dashed uuid form because the codex headers we have observed
// accept the bare hex variant and persisting the simpler form keeps the
// file format deterministic. The bytes come from crypto/rand so they
// remain unique across hosts.
func generateInstallationID() (string, error) {
	log := codexConcernLog.Logger()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Warn("adapter.codex.installation_id.rand_failed", "concern", "adapter.providers.codex.request", "subcomponent", "codex",
			"err", err.Error(),
		)
		return "", fmt.Errorf("codex installation id rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func persistInstallationID(path, id string) error {
	log := codexConcernLog.Logger()
	if path == "" {
		return errors.New("codex installation id path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Warn("adapter.codex.installation_id.mkdir_failed", "concern", "adapter.providers.codex.request", "subcomponent", "codex",
			"path", filepath.Dir(path),
			"err", err.Error(),
		)
		return fmt.Errorf("codex installation id mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		log.Warn("adapter.codex.installation_id.write_failed", "concern", "adapter.providers.codex.request", "subcomponent", "codex",
			"path", path,
			"id_len", len(id),
			"err", err.Error(),
		)
		return fmt.Errorf("codex installation id write: %w", err)
	}
	return nil
}
