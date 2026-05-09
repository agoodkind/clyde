// Package discovery scans Codex rollout artifacts for adoptable sessions.
package discovery

import (
	"path/filepath"

	codexprovider "goodkind.io/clyde/internal/providers/codex"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/session"
)

const primaryArtifactKindRollout = "rollout"

type Scanner struct {
	codexHome string
}

func NewScanner(codexHome string) Scanner {
	return Scanner{codexHome: codexHome}
}

func (s Scanner) Provider() session.ProviderID {
	return session.ProviderCodex
}

func (s Scanner) Scan() ([]session.DiscoveryResult, error) {
	paths, err := codexstore.ResolveStorePaths(s.codexHome, "")
	if err != nil {
		return nil, err
	}
	results, err := codexstore.NewDiscoveryScanner(paths).Scan()
	if err != nil {
		return nil, err
	}
	out := make([]session.DiscoveryResult, 0, len(results))
	for _, result := range results {
		out = append(out, discoveryResult(result))
	}
	return out, nil
}

func (s Scanner) DiscoveryScannerForHome(homeDir string) session.DiscoveryScanner {
	return NewScanner(defaultCodexHome(homeDir))
}

func discoveryResult(result codexstore.DiscoveryResult) session.DiscoveryResult {
	workspace := result.LatestWorkDir
	if workspace == "" {
		workspace = result.WorkspaceRoot
	}
	return session.DiscoveryResult{
		Provider: session.ProviderCodex,
		Identity: session.ProviderSessionID{
			Provider: session.ProviderCodex,
			ID:       result.ThreadID,
		},
		WorkspaceRoot:  workspace,
		Entrypoint:     result.Entrypoint,
		FirstEntryTime: result.CreatedAt,
		NameContract:   codexprovider.ThreadName{Name: result.ThreadName},
		ForkParent: session.ProviderSessionID{
			Provider: session.ProviderCodex,
			ID:       result.ForkParentID,
		},
		IsForked:            result.ForkParentID != "",
		IsSubagent:          result.IsSubagent,
		PrimaryArtifact:     result.RolloutPath,
		PrimaryArtifactKind: primaryArtifactKindRollout,
	}
}

func defaultCodexHome(homeDir string) string {
	return filepath.Join(homeDir, ".codex")
}
