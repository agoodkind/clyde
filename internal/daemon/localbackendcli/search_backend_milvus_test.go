//go:build milvusintegration

package localbackendcli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

func TestSearchBackendMilvusCLIAndMCP(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	seedBackendLocalIndex(t, binaryPath, home, localRoot)
	settings := fmt.Sprintf(
		deletedSourceSettingsTemplate,
		address,
		baseURL,
		deletedSourceEmbeddingModel,
		deletedSourceEmbeddingDim,
	)
	settings += fmt.Sprintf("local_root = %q\n", localRoot)
	settingsPath := writeSandboxSemanticSettings(t, settings)
	daemon := startLocalProofDaemon(
		t,
		binaryPath,
		home,
		"--ingestion-enabled",
		"--search-enabled",
		"--semantic-settings",
		settingsPath,
	)
	waitForProofRowsFrom(t, daemon, localProofSourceSemantic)
	for _, surface := range daemon.surfaces() {
		request := localProofSearchRequest{Query: localProofQuery, Limit: localProofPageLimit}
		configured := mustLocalProofSearch(t, surface, request)
		explicit := explicitBackendSurface(surface, "milvus")
		selected := mustLocalProofSearch(t, explicit, request)
		if selected.Source != localProofSourceSemantic ||
			!slices.Equal(
				localProofMatchKeys(configured.Matches),
				localProofMatchKeys(selected.Matches),
			) {
			t.Fatalf(
				"%s omitted and explicit Milvus results differ: %+v %+v",
				surface.name,
				configured,
				selected,
			)
		}
		assertLocalProofFilters(t, explicit, localProofSourceSemantic)
		assertLocalProofPages(t, explicit, localProofSourceSemantic)
		assertBackendScope(t, explicit, localProofSourceSemantic)
		assertBackendScope(t, explicitBackendSurface(surface, "local"), localProofSourceLocal)
	}
}
