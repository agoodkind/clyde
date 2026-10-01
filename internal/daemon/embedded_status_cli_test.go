package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/lm-semantic-search/library"
)

// TestDaemonStatusCommandReportsEmbeddedBackend opens the embedded library
// with the embedded vector store, blocks one owner with a real
// library.ErrStaleGeneration, records one pending projection, and publishes
// the outbox state the way the sync worker does after a pass. It then serves
// the daemon status RPC under an embedded ingestion configuration and runs the
// built clyde binary. The text and JSON status must report the embedded
// connection, an open library, the pending counts, and the blocked
// conversation ID.
func TestDaemonStatusCommandReportsEmbeddedBackend(t *testing.T) {
	binary := buildConversationSearchCLI(t)
	store, _ := openBlockedTestStore(t)
	ctx := t.Context()
	recordBlockedTestBatch(t, store)
	if _, err := store.library.Apply(ctx, library.Batch{
		Namespace:        store.namespace.ID,
		OwnerID:          blockedBatchOwnerID,
		GenerationOrder:  2,
		IdempotencyToken: "external-generation-2",
		Mode:             library.Append,
		Rows:             nil,
	}); err != nil {
		t.Fatalf("commit external generation 2: %v", err)
	}
	if _, err := store.delivery.replayPending(ctx); err != nil {
		t.Fatalf("replay batches: %v", err)
	}
	pendingProjection := embeddedOutboxProjection{
		Namespace:       store.namespace.ID,
		OwnerID:         blockedProjectionOwnerID,
		ProjectionOrder: 1,
		Token:           "pending-projection-1",
		Metadata:        embeddedOwnerMetadata{Provider: "codex", WorkspaceRoot: "", Archived: false, Subagent: false},
	}
	if err := store.outbox.recordProjection(ctx, pendingProjection, nil); err != nil {
		t.Fatalf("record projection: %v", err)
	}
	status := newEmbeddedSemanticStatus()
	status.publishStore(ctx, store)

	cfg := config.NewConfig()

	cfg.Conversation.Semantic.IngestionEnabled = true
	socket := conversationSearchSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeServices{listener: listener, embeddedStatus: status}
	runtime.currentConfig.Store(cfg)
	index := conversation.NewIndex(conversation.NewRegistry(), cfg.Conversation)
	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, newControlServer(cfg, semanticTestLogger(), nil, index, nil, grpcServer, runtime, exportTokenConfig{}))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	configRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configRoot, "clyde"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[daemon]\ngrpc_address = %q\n", "unix://"+socket)
	if err := os.WriteFile(filepath.Join(configRoot, "clyde", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := environmentWithOverrides("HOME="+t.TempDir(), "XDG_CONFIG_HOME="+configRoot, "XDG_RUNTIME_DIR="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())

	command := exec.CommandContext(ctx, binary, "daemon", "status")
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, output)
	}
	for _, want := range []string{
		"ingestion_enabled=true search_enabled=false connection=embedded",
		"semantic_embedded: library_open=true pending_batches=0 pending_projections=1 blocked_owners=1 blocked_conversation_ids=" + blockedBatchOwnerID,
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("status missing %q:\n%s", want, output)
		}
	}

	command = exec.CommandContext(ctx, binary, "daemon", "status", "--output-format", "json")
	command.Env = environment
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("JSON status: %v\n%s", err, output)
	}
	var decoded struct {
		Runtime *RuntimeStatus `json:"runtime"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode status: %v\n%s", err, output)
	}
	embedded := decoded.Runtime.Semantic.Embedded
	if decoded.Runtime.Semantic.Connection != "embedded" || embedded == nil || !embedded.LibraryOpen ||
		embedded.PendingProjections != 1 || embedded.BlockedOwners != 1 ||
		!slices.Equal(embedded.BlockedConversationIDs, []string{blockedBatchOwnerID}) {
		t.Fatalf("JSON status = %s", output)
	}
}
