//go:build live

package live

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/config"
)

func TestResolveFakeConversationSemanticConfigDefaults(t *testing.T) {
	t.Setenv("CLYDE_TEST_CONVERSATION_SEMANTIC", "")
	t.Setenv("CLYDE_TEST_COLLECTION_ID", "")

	first := resolveFakeConversationSemanticConfig(t)
	second := resolveFakeConversationSemanticConfig(t)

	if first.Enabled {
		t.Fatal("Enabled = true, want false")
	}
	if first.CollectionID == "" {
		t.Fatal("CollectionID = empty, want random id")
	}
	if second.CollectionID == "" {
		t.Fatal("second CollectionID = empty, want random id")
	}
	if first.CollectionID == second.CollectionID {
		t.Fatalf("CollectionID = %q twice, want a fresh random id per resolution", first.CollectionID)
	}
}

func TestResolveFakeConversationSemanticConfigHonorsEnv(t *testing.T) {
	t.Setenv("CLYDE_TEST_CONVERSATION_SEMANTIC", "true")
	t.Setenv("CLYDE_TEST_COLLECTION_ID", "test-collection")

	cfg := resolveFakeConversationSemanticConfig(t)
	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.CollectionID != "test-collection" {
		t.Fatalf("CollectionID = %q, want %q", cfg.CollectionID, "test-collection")
	}
}

func TestWriteConfigCarriesConversationSemanticSettings(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		write func(*testing.T, *harness)
	}{
		{
			name: "mitm only",
			write: func(t *testing.T, h *harness) {
				t.Helper()
				h.writeConfig(t, h.cfg.MITMPort, []string{"anthropic"})
			},
		},
		{
			name: "adapter",
			write: func(t *testing.T, h *harness) {
				t.Helper()
				h.writeAdapterConfig(t, h.cfg.AdapterPort, "http://[::1]:18080", nil)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			configRoot := t.TempDir()
			h := &harness{
				stateRoot:   root,
				configRoot:  configRoot,
				runtimeRoot: t.TempDir(),
				cfg: fakePorts{
					MITMPort:      fakeMITMPort,
					AdapterPort:   fakeAdapterPort,
					CursorPort:    fakeCursorPort,
					TopologyPort:  fakeTopologyPort,
					MovedMITMPort: fakeMITMPort + 1,
				},
				conversationSemantic: fakeConversationSemanticConfig{
					Enabled:      true,
					CollectionID: "test-collection",
				},
				binPath:      "",
				configPath:   filepath.Join(configRoot, "clyde", "config.toml"),
				daemonLog:    "",
				prodPidsPre:  nil,
				extraEnv:     nil,
				requireToken: "",
				cmd:          nil,
			}
			if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
				t.Fatalf("mkdir config dir: %v", err)
			}

			testCase.write(t, h)

			body, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}

			// Decode the written file the way the daemon will rather than
			// searching it for strings. A substring is satisfied by a value in
			// the wrong section, and by a file whose syntax the daemon would
			// reject, so it can pass for a config the daemon cannot boot on.
			var written config.Config
			if err := toml.Unmarshal(body, &written); err != nil {
				t.Fatalf("parse config: %v\n%s", err, body)
			}
			semantic := written.Conversation.Semantic
			if !semantic.FeedsEngine() {
				t.Fatalf("FeedsEngine() = false, want true:\n%s", body)
			}
			if !semantic.AnswersSearch() {
				t.Fatalf("AnswersSearch() = false, want true:\n%s", body)
			}
			if semantic.CollectionID != "test-collection" {
				t.Fatalf("CollectionID = %q, want %q:\n%s", semantic.CollectionID, "test-collection", body)
			}
		})
	}
}
