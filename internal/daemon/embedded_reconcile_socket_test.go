package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// TestReconcileCommandRunsInsideDaemonWorker serves the daemon control socket
// under temporary roots with an embedded ingestion worker state attached to
// the reconcile gate. The worker store blocks one owner with a real
// library.ErrStaleGeneration. The built clyde binary runs
// `clyde daemon reconcile-embedded-conversation` against that socket. The
// daemon must reconcile the owner with its own store and clear the blocked
// state. The same command against a daemon without an embedded worker must
// fail with the worker error and leave the outbox unchanged.
func TestReconcileCommandRunsInsideDaemonWorker(t *testing.T) {
	binary := buildConversationSearchCLI(t)
	stores := isolateEmbeddedProjectionStores(t)
	ageLiveArtifactForGate(t, writeEmbeddedProjectionCodexRollout(t, stores))
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	store, _ := openBlockedTestStore(t)
	records := reconcileTestRecords(t, index)
	generation := prepareReconcileTestGeneration(t, store, records[reconcileKnownOwnerID])
	if err := store.outbox.recordBatch(t.Context(), generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	commitReconcileTestGeneration(t, store, reconcileKnownOwnerID, reconcileExternalOrderTwo, reconcileExternalTokenTwo)
	if _, err := store.delivery.replayPending(t.Context()); err != nil {
		t.Fatalf("replay batches: %v", err)
	}

	withoutWorker := newEmbeddedReconcileGate(nil)
	output, err := runReconcileCommand(t, binary, index, withoutWorker)
	if err == nil || !strings.Contains(output, "runs no embedded conversation ingestion worker") {
		t.Fatalf("reconcile without a worker = %v:\n%s", err, output)
	}
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 1 {
		t.Fatalf("blocked owners after the refused command = %q, %v, want the owner", blocked, err)
	}

	semantic := config.ConversationSemanticConfig{
		IngestionEnabled: true,
		CollectionID:     store.namespace.ID,
		Backend:          config.ConversationSemanticBackendEmbedded,
	}
	worker := newEmbeddedConversationSync(semantic, "", newEmbeddedSemanticStatus(), index)
	worker.store = store
	withWorker := newEmbeddedReconcileGate(nil)
	withWorker.attach(worker)
	output, err = runReconcileCommand(t, binary, index, withWorker)
	want := fmt.Sprintf("Reconciled %s: 0 published rows, projection order 0, 1 aborted tokens.", reconcileKnownOwnerID)
	if err != nil || !strings.Contains(output, want) {
		t.Fatalf("reconcile with a worker = %v, want %q:\n%s", err, want, output)
	}
	blocked, err = store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked owners after the reconcile command = %q, %v, want none", blocked, err)
	}
}

// runReconcileCommand serves the control socket with gate and runs the
// reconcile command of binary against it.
func runReconcileCommand(t *testing.T, binary string, index *conversation.Index, gate *embeddedReconcileGate) (string, error) {
	t.Helper()
	cfg := config.NewConfig()
	cfg.Conversation.Semantic.Backend = config.ConversationSemanticBackendEmbedded
	cfg.Conversation.Semantic.IngestionEnabled = true
	socket := conversationSearchSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeServices{listener: listener, embeddedStatus: newEmbeddedSemanticStatus(), embeddedReconcile: gate}
	runtime.currentConfig.Store(cfg)
	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, newControlServer(cfg, semanticTestLogger(), nil, index, nil, grpcServer, runtime, exportTokenConfig{}))
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	configRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configRoot, "clyde"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[daemon]\ngrpc_address = %q\n", "unix://"+socket)
	if err := os.WriteFile(filepath.Join(configRoot, "clyde", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "daemon", "reconcile-embedded-conversation", "--conversation", reconcileKnownOwnerID)
	command.Env = environmentWithOverrides("HOME="+t.TempDir(), "XDG_CONFIG_HOME="+configRoot, "XDG_RUNTIME_DIR="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
	output, err := command.CombinedOutput()
	return string(output), err
}
