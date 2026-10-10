package localbackendcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	clidaemon "goodkind.io/clyde/internal/cli/daemon"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemon/localtest"
)

const (
	localProofQuery           = "why does the listener rebind after the watcher classifies a config change"
	localProofPageLimit       = 4
	localProofMaxLimit        = 50
	localProofReadyTimeout    = 3 * time.Minute
	localProofCommandTimeout  = time.Minute
	localProofPollDelay       = 250 * time.Millisecond
	localProofScannerBytes    = 16 << 20
	localProofSearchRequestID = 2

	localProofSourceLocal    = "local"
	localProofSourceRawText  = "raw_text"
	localProofSourceSemantic = "semantic"
	localProofUpsertEvent    = "conversation.vectorsearch.upsert_completed"

	localProofProviderClaude = "claude"
	localProofProviderCodex  = "codex"
	localProofRoleUser       = "user"
	localProofRoleAssistant  = "assistant"

	localProofAlphaSession     = "local-proof-alpha-session"
	localProofBetaSession      = "local-proof-beta-session"
	localProofAlphaWorkspace   = "/local-proof/alpha"
	localProofBetaWorkspace    = "/local-proof/beta"
	localProofActiveRollout    = "019e0000-3a00-7010-bd9f-a6ee71550001"
	localProofArchivedRollout  = "019e0000-3a00-7010-bd9f-a6ee71550002"
	localProofActiveWorkspace  = "/local-proof/gamma"
	localProofArchiveWorkspace = "/local-proof/archived"

	localProofClaudeTurns     = 3
	localProofCodexTurns      = 2
	localProofMessagesPerTurn = 2
	localProofClaudeSessions  = 2
	localProofClaudeRows      = localProofClaudeSessions * localProofClaudeTurns * localProofMessagesPerTurn
	localProofSessionRows     = localProofClaudeTurns * localProofMessagesPerTurn
	localProofRolloutRows     = localProofCodexTurns * localProofMessagesPerTurn
	localProofVisibleRows     = localProofClaudeRows + localProofRolloutRows
	localProofAllRows         = localProofVisibleRows + localProofRolloutRows
	localProofVisibleUserRows = localProofVisibleRows / localProofMessagesPerTurn
	localProofVisibleSessions = localProofClaudeSessions + 1
	localProofAllSessions     = localProofVisibleSessions + 1
	localProofDaemonLogName   = "clyde-daemon.jsonl"

	localProofRawTextSessions         = 2
	localProofUnreachableMilvusConfig = `[conversation.semantic]
ingestion_enabled = false
search_enabled = true
milvus_address = %q
embedding_base_url = "http://%s/v1"

[adapter]
enabled = false

[mitm]
enabled_default = false
`

	localProofInitializeRequest  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"clyde-local-backend-proof","version":"test"}}}`
	localProofInitializedMessage = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
)

func TestLocalBackendCLIAndMCP(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	localFlags := []string{"--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled"}

	first := startLocalProofDaemon(t, binaryPath, home, localFlags...)
	waitForLocalProofRows(t, first)

	t.Run("source and status report the local backend", func(t *testing.T) {
		for _, surface := range first.surfaces() {
			result := mustLocalProofSearch(t, surface, localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false})
			if result.Source != localProofSourceLocal {
				t.Errorf("%s source = %q, want %q", surface.name, result.Source, localProofSourceLocal)
			}
		}
		status := first.status(t)
		for _, expected := range []string{"backend=local", "connection=ready", "ingestion_enabled=true search_enabled=true"} {
			if !strings.Contains(status, expected) {
				t.Errorf("daemon status missing %q:\n%s", expected, status)
			}
		}
	})

	t.Run("filters exclude ineligible conversations", func(t *testing.T) {
		for _, surface := range first.surfaces() {
			assertLocalProofFilters(t, surface)
		}
	})

	t.Run("offset pages partition one ranking", func(t *testing.T) {
		for _, surface := range first.surfaces() {
			assertLocalProofPages(t, surface)
		}
	})

	t.Run("local answers with no Milvus address and no embedding endpoint", func(t *testing.T) {
		configText := first.config(t)
		if !strings.Contains(configText, `backend = "local"`) {
			t.Errorf("sandbox config does not select the local backend:\n%s", configText)
		}
		for _, endpointKey := range []string{"milvus_address", "embedding_base_url", "embedding_api_key", "socket_path"} {
			if strings.Contains(configText, endpointKey) {
				t.Errorf("sandbox config sets %s:\n%s", endpointKey, configText)
			}
		}
		for _, surface := range first.surfaces() {
			result := mustLocalProofSearch(t, surface, localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofPageLimit, Offset: 0, IncludeArchived: false})
			if result.Source != localProofSourceLocal || len(result.Matches) != localProofPageLimit {
				t.Errorf("%s returned source %q with %d matches, want %q with %d", surface.name, result.Source, len(result.Matches), localProofSourceLocal, localProofPageLimit)
			}
		}
	})

	fullRequest := localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true}
	rankingBefore := make(map[string][]localProofMatchKey)
	for _, surface := range first.surfaces() {
		rankingBefore[surface.name] = localProofMatchKeys(mustLocalProofSearch(t, surface, fullRequest).Matches)
	}
	firstUpserts := readLocalProofUpserts(t, home, first.roots, time.Time{})
	storeBefore := localProofStoreDigests(t, localRoot)
	first.stop()

	t.Run("restart on the same root keeps the ranking and writes no rows", func(t *testing.T) {
		firstRowsWritten := 0
		for _, record := range firstUpserts {
			firstRowsWritten += record.RowsWritten
		}
		if firstRowsWritten != localProofAllRows {
			t.Fatalf("the first daemon logged %d written rows in %s records, want %d: %+v", firstRowsWritten, localProofUpsertEvent, localProofAllRows, firstUpserts)
		}
		if len(storeBefore) == 0 {
			t.Fatalf("the first daemon wrote no store file under %s", localRoot)
		}

		restartedAt := time.Now()
		second := startLocalProofDaemon(t, binaryPath, home, localFlags...)
		waitForLocalProofRows(t, second)
		for _, surface := range second.surfaces() {
			result := mustLocalProofSearch(t, surface, fullRequest)
			if result.Source != localProofSourceLocal {
				t.Errorf("%s source after the restart = %q, want %q", surface.name, result.Source, localProofSourceLocal)
			}
			rankingAfter := localProofMatchKeys(result.Matches)
			if !slices.Equal(rankingAfter, rankingBefore[surface.name]) {
				t.Errorf("%s ranking changed across the restart:\nbefore %v\nafter  %v", surface.name, rankingBefore[surface.name], rankingAfter)
			}
		}

		deadline := time.Now().Add(localProofReadyTimeout)
		var secondUpserts []localProofUpsertRecord
		for {
			secondUpserts = readLocalProofUpserts(t, home, second.roots, restartedAt)
			delivered := 0
			for _, record := range secondUpserts {
				delivered += record.Conversations
			}
			if delivered >= localProofAllSessions {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the restarted daemon logged %d delivered conversations in %s records, want %d; stderr:\n%s", delivered, localProofUpsertEvent, localProofAllSessions, second.stderr.String())
			}
			time.Sleep(localProofPollDelay)
		}
		for _, record := range secondUpserts {
			if record.RowsWritten != 0 || record.VectorsEmbedded != 0 || len(record.WrittenConversations) != 0 {
				t.Errorf("the restarted daemon rebuilt rows for unchanged conversations: %+v", record)
			}
		}
		second.stop()
		storeAfter := localProofStoreDigests(t, localRoot)
		if !maps.Equal(storeAfter, storeBefore) {
			t.Errorf("the local store files changed across the restart:\nbefore %v\nafter  %v", storeBefore, storeAfter)
		}
	})

	t.Run("disabled search answers from raw text", func(t *testing.T) {
		disabledRoot := filepath.Join(t.TempDir(), "local-index")
		disabled := startLocalProofDaemon(t, binaryPath, home, "--local", "--local-root", disabledRoot, "--ingestion-enabled")
		assertLocalProofRawText(t, disabled)
		status := disabled.status(t)
		for _, expected := range []string{"backend=local", "search_enabled=false"} {
			if !strings.Contains(status, expected) {
				t.Errorf("daemon status missing %q:\n%s", expected, status)
			}
		}
	})

	t.Run("an unreadable local root answers from raw text", func(t *testing.T) {
		unreadableRoot := filepath.Join(t.TempDir(), "local-index")
		if err := os.WriteFile(unreadableRoot, []byte("not a directory\n"), 0o600); err != nil {
			t.Fatalf("write a file at the local root: %v", err)
		}
		unreadable := startLocalProofDaemon(t, binaryPath, home, "--local", "--local-root", unreadableRoot, "--ingestion-enabled", "--search-enabled")
		assertLocalProofRawText(t, unreadable)
		status := unreadable.status(t)
		for _, expected := range []string{"backend=local", "connection=unavailable"} {
			if !strings.Contains(status, expected) {
				t.Errorf("daemon status missing %q:\n%s", expected, status)
			}
		}
	})

	t.Run("a sandbox without --local never reports local", func(t *testing.T) {
		plain := startLocalProofDaemon(t, binaryPath, home)
		assertLocalProofRawText(t, plain)
		status := plain.status(t)
		if !strings.Contains(status, "backend=milvus") {
			t.Errorf("daemon status missing %q:\n%s", "backend=milvus", status)
		}
		if strings.Contains(status, "backend=local") {
			t.Errorf("daemon status reports the local backend without --local:\n%s", status)
		}
		if configText := plain.config(t); strings.Contains(configText, "backend") {
			t.Errorf("sandbox config selects a backend without --local:\n%s", configText)
		}
	})

	t.Run("search enabled with unreachable Milvus never reports local", func(t *testing.T) {
		closedAddress := localProofClosedAddress(t)
		localtest.StartRawTextDaemon(t, fmt.Sprintf(localProofUnreachableMilvusConfig, closedAddress, closedAddress))
		query := "watcher config"
		waitForFirstMatches(t, conversation.SearchConversationsOptions{Query: query, Limit: localProofMaxLimit}, localProofRawTextSessions)

		report := daemon.InspectStatus(context.Background())
		if report.Runtime == nil {
			t.Fatalf("daemon status has no runtime snapshot: %s", report.DaemonError)
		}
		var statusText bytes.Buffer
		clidaemon.WriteRuntimeStatusReport(&statusText, report.Runtime)
		for _, expected := range []string{"search_enabled=true", "backend=milvus", "connection=unavailable"} {
			if !strings.Contains(statusText.String(), expected) {
				t.Errorf("daemon status missing %q:\n%s", expected, statusText.String())
			}
		}
		if strings.Contains(statusText.String(), "backend=local") {
			t.Errorf("daemon status reports the local backend with backend unset:\n%s", statusText.String())
		}

		registry := clispec.NewConversationRegistry()
		var cliResult localProofSearchOutput
		cliOutput := localtest.LocalBackendSearchCLIOutput(t, registry, query)
		if err := json.Unmarshal([]byte(cliOutput), &cliResult); err != nil {
			t.Fatalf("decode conversation search output: %v\n%s", err, cliOutput)
		}
		if cliResult.Source != localProofSourceRawText || len(cliResult.Matches) != localProofRawTextSessions {
			t.Errorf("conversation search returned source %q with %d matches, want %q with %d", cliResult.Source, len(cliResult.Matches), localProofSourceRawText, localProofRawTextSessions)
		}

		var mcpResponse localProofRPCResponse
		mcpOutput := localtest.LocalBackendSearchMCPOutput(t, registry, query)
		if err := json.Unmarshal([]byte(mcpOutput), &mcpResponse); err != nil {
			t.Fatalf("decode clyde_search response: %v\n%s", err, mcpOutput)
		}
		if strings.Contains(mcpOutput, localtest.LocalBackendSourceJSON) {
			t.Errorf("clyde_search reports the local source with backend unset:\n%s", mcpOutput)
		}
		if mcpResponse.Result == nil || mcpResponse.Result.IsError || mcpResponse.Result.StructuredContent == nil {
			t.Fatalf("clyde_search returned no structured result:\n%s", mcpOutput)
		}
		mcpResult := *mcpResponse.Result.StructuredContent
		if mcpResult.Source != localProofSourceRawText || len(mcpResult.Matches) != localProofRawTextSessions {
			t.Errorf("clyde_search returned source %q with %d matches, want %q with %d", mcpResult.Source, len(mcpResult.Matches), localProofSourceRawText, localProofRawTextSessions)
		}
	})
}
