package localbackendcli_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func explicitBackendSurface(surface localProofSurface, backend string) localProofSurface {
	return localProofSurface{
		name: surface.name + " backend=" + backend,
		search: func(request localProofSearchRequest) (localProofSearchOutput, error) {
			request.Backend = backend
			return surface.search(request)
		},
	}
}

func assertBackendScope(t *testing.T, surface localProofSurface, source string) {
	t.Helper()
	conversationID := localProofProviderClaude + ":" + localProofAlphaSession
	request := localProofSearchRequest{
		Query:          localProofQuery,
		ConversationID: conversationID,
		Provider:       localProofProviderClaude,
		Workspace:      localProofAlphaWorkspace,
		Roles:          localProofRoleAssistant,
		After:          "2026-07-01T12:00:00Z",
		Before:         "2026-07-01T12:00:06Z",
		Limit:          localProofMaxLimit,
	}
	result := mustLocalProofSearch(t, surface, request)
	if result.Source != source || len(result.Matches) != localProofClaudeTurns {
		t.Fatalf("%s returned source %q and %d scoped matches", surface.name, result.Source, len(result.Matches))
	}
	for _, match := range result.Matches {
		if match.Conversation.ID != conversationID || match.Role != localProofRoleAssistant {
			t.Errorf("%s returned a match outside the conversation and role scope: %+v", surface.name, match)
		}
	}
}

func assertBackendError(t *testing.T, surface localProofSurface, backend string, expected string) {
	t.Helper()
	_, err := surface.search(localProofSearchRequest{Query: localProofQuery, Backend: backend, Limit: localProofPageLimit})
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("%s backend %q error = %v, want %q", surface.name, backend, err, expected)
	}
}

func seedBackendLocalIndex(t *testing.T, binaryPath string, home string, localRoot string) {
	t.Helper()
	settingsPath := writeSandboxSemanticSettings(t, "")
	seed := startLocalProofDaemon(t, binaryPath, home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	waitForLocalProofRows(t, seed)
	for _, surface := range seed.surfaces() {
		request := localProofSearchRequest{Query: localProofQuery, Limit: localProofPageLimit}
		configured := mustLocalProofSearch(t, surface, request)
		explicit := mustLocalProofSearch(t, explicitBackendSurface(surface, "local"), request)
		if configured.Source != localProofSourceLocal || explicit.Source != localProofSourceLocal || !slices.Equal(localProofMatchKeys(configured.Matches), localProofMatchKeys(explicit.Matches)) {
			t.Fatalf("%s omitted and explicit local results differ: %+v %+v", surface.name, configured, explicit)
		}
	}
	seed.stop()
}

func TestSearchBackendCLIAndMCP(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	seedBackendLocalIndex(t, binaryPath, home, localRoot)
	before := localProofStoreDigests(t, localRoot)
	settingsPath := writeSandboxSemanticSettings(t, fmt.Sprintf("milvus_address = %q\nlocal_root = %q\n", localProofRefusedAddress, localRoot))
	daemon := startLocalProofDaemon(t, binaryPath, home, "--search-enabled", "--semantic-settings", settingsPath)
	assertLocalProofRawText(t, daemon)
	for _, surface := range daemon.surfaces() {
		explicit := explicitBackendSurface(surface, "local")
		assertLocalProofFilters(t, explicit, localProofSourceLocal)
		assertLocalProofPages(t, explicit, localProofSourceLocal)
		assertBackendScope(t, explicit, localProofSourceLocal)
		assertBackendError(t, surface, "milvus", "search backend milvus: conversation_search_source_unavailable")
		assertBackendError(t, surface, "unknown", "invalid search backend")
		_, err := surface.search(localProofSearchRequest{Backend: "local", Limit: localProofPageLimit})
		if err == nil || !strings.Contains(err.Error(), "backend requires a query") {
			t.Fatalf("%s backend without query error = %v", surface.name, err)
		}
	}
	daemon.stop()
	if after := localProofStoreDigests(t, localRoot); !maps.Equal(before, after) {
		t.Fatal("explicit local searches changed the existing index files")
	}

	t.Run("missing local index", func(t *testing.T) {
		missingRoot := filepath.Join(t.TempDir(), "absent-index")
		settings := writeSandboxSemanticSettings(t, fmt.Sprintf("milvus_address = %q\nlocal_root = %q\n", localProofRefusedAddress, missingRoot))
		missing := startLocalProofDaemon(t, binaryPath, home, "--search-enabled", "--semantic-settings", settings)
		for _, surface := range missing.surfaces() {
			assertBackendError(t, surface, "local", "search backend local: conversation_search_source_unavailable")
		}
		if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
			t.Fatalf("query created the missing local index: %v", err)
		}
	})

	t.Run("missing collection", func(t *testing.T) {
		emptyRoot := t.TempDir()
		settings := writeSandboxSemanticSettings(t, fmt.Sprintf("milvus_address = %q\nlocal_root = %q\n", localProofRefusedAddress, emptyRoot))
		empty := startLocalProofDaemon(t, binaryPath, home, "--search-enabled", "--semantic-settings", settings)
		for _, surface := range empty.surfaces() {
			assertBackendError(t, surface, "local", "search backend local: conversation_search_source_unavailable")
		}
		entries, err := os.ReadDir(emptyRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("query changed the empty local index: entries=%v error=%v", entries, err)
		}
	})
}
