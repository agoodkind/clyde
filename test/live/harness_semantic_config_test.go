//go:build live

package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			text := string(body)
			for _, want := range []string{
				"[conversation.semantic]",
				"enabled = true",
				"search_enabled = true",
				`collection_id = "test-collection"`,
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("config missing %q:\n%s", want, text)
				}
			}
		})
	}
}
