package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// TestHardResetPreservesEmbeddedSemanticStore configures the embedded search
// catalog inside the state logs tree and the writer lock at the state update
// lock path, which the state and default reset scopes remove. It also writes
// an outbox under the conversation-semantic state directory. HardResetWithOptions
// must refuse both scopes before any service change and leave all three files
// unchanged.
func TestHardResetPreservesEmbeddedSemanticStore(t *testing.T) {
	for _, scope := range []HardResetScope{HardResetScopeAll, HardResetScopeState} {
		t.Run("scope_"+string(scope), func(t *testing.T) {
			resetTestRoots(t)
			state := config.DefaultStateDir()
			catalogPath := filepath.Join(state, "logs", "semantic", "catalog.sqlite")
			lockPath := filepath.Join(state, "update.lock")
			outboxPath := filepath.Join(state, "conversation-semantic", "outbox-pool.sqlite")
			preserved := map[string][]byte{
				catalogPath: []byte("committed occurrence catalog"),
				lockPath:    []byte("catalog writer lock"),
				outboxPath:  []byte("outbox batches"),
			}
			for path, body := range preserved {
				writeResetFixture(t, path, body)
			}
			configBody := []byte("[conversation.semantic]\ncatalog_path = " + strconv.Quote(catalogPath) + "\nlock_path = " + strconv.Quote(lockPath) + "\n")
			writeResetFixture(t, config.GlobalConfigPath(), configBody)
			cfg, err := config.LoadGlobalOrDefault()
			if err != nil {
				t.Fatal(err)
			}
			// The test stops here when the inventory accepts a preserved path.
			// A red run then never calls the service manager.
			if _, err := hardResetTargetsForScope(t.Context(), cfg, scope); err == nil {
				t.Fatalf("scope %q inventory accepted the embedded catalog or lock for deletion", scope)
			}
			var output bytes.Buffer
			err = HardResetWithOptions(t.Context(), &output, HardResetOptions{Scope: scope})
			if err == nil || !strings.Contains(err.Error(), "protected") {
				t.Fatalf("hard reset error = %v, want a protected-path refusal", err)
			}
			if output.Len() != 0 {
				t.Fatalf("service removal ran before refusal: %s", &output)
			}
			for path, want := range preserved {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("preserved bytes changed at %s: %v", path, err)
				}
			}
		})
	}
}
