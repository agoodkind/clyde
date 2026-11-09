package statusproof_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const parallelProofUnavailableConfig = `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
backend = "milvus"
backends = ["milvus", "local"]
milvus_address = "localhost:1"
sync_interval = "1s"
index_refresh_interval = "1s"

[adapter]
enabled = false

[mitm]
enabled_default = false
`

func waitForParallelProofMatch(
	t *testing.T,
	backend conversation.SearchBackend,
	session string,
	token string,
) conversation.SearchConversationsResult {
	t.Helper()
	deadline := time.Now().Add(ingestionProofFailureTimeout)
	var last conversation.SearchConversationsResult

	for time.Now().Before(deadline) {
		result, err := daemon.SearchConversations(
			t.Context(),
			conversation.SearchConversationsOptions{
				Backend: backend,
				Query:   token,
				Limit:   ingestionProofSearchLimit,
			},
		)
		if err != nil && status.Code(err) != codes.Unavailable &&
			status.Code(err) != codes.FailedPrecondition {
			t.Fatal(err)
		}

		last = result
		if err == nil && result.Freshness.LastSyncUnix > 0 &&
			ingestionProofHasMatch(result, session, token) {
			return result
		}

		time.Sleep(ingestionProofPollInterval)
	}

	t.Fatalf("backend=%s session=%s matches=%s", backend, session, ingestionProofDescribe(last))

	return last
}

func assertParallelProofLocalCommands(t *testing.T) {
	t.Helper()
	registry := clispec.NewConversationRegistry()
	var stdout bytes.Buffer
	factory := cli.NewSystemFactory(cli.BuildInfo{})
	factory.IOStreams = &cli.IOStreams{In: strings.NewReader(""), Out: &stdout, Err: &stdout}
	root := &cobra.Command{Use: "clyde"}
	output.PersistentFlag(root)
	root.AddCommand(clispec.RenderCobra(registry, factory)...)
	root.SetArgs([]string{
		"--output-format",
		"json",
		"conversation",
		"search",
		"--query",
		ingestionProofFirstToken,
		"--backend",
		"local",
	})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(stdout.String(), `"source":"local"`) ||
		!strings.Contains(stdout.String(), ingestionProofFirstToken) {
		t.Fatalf("CLI local response: %s", stdout.String())
	}

	mcp := server.NewMCPServer("parallel-proof", "test")
	clispec.RenderMCP(registry, mcp)
	request := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"clyde_search","arguments":{"query":%q,"backend":"local"}}}`,
		ingestionProofFirstToken,
	)
	response, err := json.Marshal(mcp.HandleMessage(context.Background(), json.RawMessage(request)))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(response), `"source":"local"`) ||
		!strings.Contains(string(response), ingestionProofFirstToken) {
		t.Fatalf("MCP local response: %s", response)
	}
}

func TestParallelIngestionContinuesLocalWhileMilvusIsUnavailable(t *testing.T) {
	proof := startIngestionProofDaemonWithConfig(t, parallelProofUnavailableConfig)
	result := waitForParallelProofMatch(
		t,
		conversation.SearchBackendLocal,
		ingestionProofMainSession,
		ingestionProofFirstToken,
	)
	if result.Source != conversation.SearchSourceLocal || result.Freshness.Manifest != 1 {
		t.Fatalf("local search = %+v", result)
	}

	metrics := waitForStatusProof(
		t,
		"parallel",
		ingestionProofFailureTimeout,
		func(semantic daemon.SemanticStatus) bool {
			return semantic.Backend == "milvus" && semantic.NextRetryUnix != nil &&
				statusProofText(semantic.Detail, "semantic_backends.local.connection") == "ready" &&
				statusProofText(
					semantic.Detail,
					"semantic_backends.milvus.semantic_sync.state",
				) == "blocked"
		},
	).Semantic.Detail
	if statusProofInt(
		t,
		"parallel",
		metrics,
		"semantic_backends.local.semantic_sync.checkpoint.count",
	) != 1 ||
		statusProofInt(
			t,
			"parallel",
			metrics,
			"semantic_backends.milvus.semantic_freshness.embedded",
		) != 0 {
		t.Fatalf("independent progress = %+v", metrics)
	}

	proof.install(
		t,
		ingestionProofSentinelSession,
		ingestionProofSentinelNumber,
		[]ingestionProofMessage{
			{role: "user", body: ingestionProofSentinelText},
		},
		0,
	)
	waitForStatusProof(
		t,
		"partial-local",
		ingestionProofPassTimeout,
		func(semantic daemon.SemanticStatus) bool {
			pending, known := statusProofKnownInt(
				semantic.Detail,
				"semantic_backends.local.semantic_freshness.pending",
			)
			return known && pending == 1
		},
	)
	assertParallelProofLocalCommands(t)
	result = waitForParallelProofMatch(
		t,
		conversation.SearchBackendLocal,
		ingestionProofMainSession,
		ingestionProofFirstToken,
	)
	if result.Freshness.Pending != 1 || result.Freshness.Manifest != 2 {
		t.Fatalf("partial local freshness = %+v", result.Freshness)
	}

	proof.install(
		t,
		ingestionProofSentinelSession,
		ingestionProofSentinelNumber,
		[]ingestionProofMessage{
			{role: "user", body: ingestionProofSentinelText},
		},
		ingestionProofFirstAge,
	)
	waitForParallelProofMatch(
		t,
		conversation.SearchBackendLocal,
		ingestionProofSentinelSession,
		ingestionProofSentinelToken,
	)
	proof.stop()
	proof.stop = runIngestionProofDaemon(t)
	waitForParallelProofMatch(
		t,
		conversation.SearchBackendLocal,
		ingestionProofSentinelSession,
		ingestionProofSentinelToken,
	)
	metrics = waitForStatusProof(
		t,
		"restart-local",
		ingestionProofFailureTimeout,
		func(semantic daemon.SemanticStatus) bool {
			count, known := statusProofKnownInt(
				semantic.Detail,
				"semantic_backends.local.semantic_sync.checkpoint.count",
			)
			return known && count == 2
		},
	).Semantic.Detail
	if sent := statusProofInt(
		t,
		"restart-local",
		metrics,
		"semantic_backends.local.semantic_sync.totals.sent_conversations",
	); sent != 0 {
		t.Fatalf("restart sent %d unchanged conversations", sent)
	}
}
