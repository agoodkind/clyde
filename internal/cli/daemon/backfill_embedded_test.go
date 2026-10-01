package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
)

const embeddedBackfillRefusal = "is not available with conversation.semantic.backend = \"embedded\""

// TestBackfillCommandsRefuseEmbeddedBackend loads configurations from TOML
// and runs both backfill commands with --execute. The embedded backend must
// refuse each command before any index refresh or socket dial, with a message
// about automatic ReprojectScalars and append-only occurrences. The
// lm-semantic-search backend must not return that refusal.
func TestBackfillCommandsRefuseEmbeddedBackend(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		embedded     bool
		wantRefusal  bool
		newCommand   func(*cli.Factory) *cobra.Command
		commandFlags []string
	}{
		{name: "scalars_embedded", embedded: true, wantRefusal: true, newCommand: newBackfillConversationScalarsCmd, commandFlags: []string{"--execute"}},
		{name: "documents_embedded", embedded: true, wantRefusal: true, newCommand: newBackfillConversationDocumentsCmd, commandFlags: []string{"--execute", "--conversation", "codex:missing"}},
		{name: "scalars_lms", embedded: false, wantRefusal: false, newCommand: newBackfillConversationScalarsCmd, commandFlags: []string{"--execute"}},
		{name: "documents_lms", embedded: false, wantRefusal: false, newCommand: newBackfillConversationDocumentsCmd, commandFlags: []string{"--execute", "--conversation", "codex:missing"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "CODEX_HOME", "CODEX_SQLITE_HOME", "CLAUDE_CONFIG_DIR", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
				t.Setenv(key, filepath.Join(root, strings.ToLower(key)))
			}
			body := "[conversation.semantic]\ningestion_enabled = true\nsocket_path = " + strconv.Quote(filepath.Join(root, "missing.sock")) + "\n"
			if testCase.embedded {
				body += "backend = \"embedded\"\nprojection_profile = \"p3\"\ncatalog_path = " + strconv.Quote(filepath.Join(root, "catalog.sqlite")) + "\n" +
					"lock_path = " + strconv.Quote(filepath.Join(root, "catalog.lock")) + "\npool_id = \"backfill\"\n" +
					"milvus_address = \"localhost:1\"\nmilvus_database = \"clyde_backfill\"\nmilvus_collection = \"vectors\"\n" +
					"embedding_base_url = \"http://localhost:1/v1\"\nembedding_model = \"nvidia/NV-EmbedCode-7b-v1\"\n" +
					"embedding_revision = \"backfill\"\nvector_dimension = 4096\nnormalization = \"l2\"\n"
			}
			if err := os.MkdirAll(filepath.Dir(config.GlobalConfigPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config.GlobalConfigPath(), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			output := &bytes.Buffer{}
			factory := testDocumentBackfillFactory(output)
			factory.Config = config.LoadGlobalOrDefault
			command := testCase.newCommand(factory)
			command.SetArgs(testCase.commandFlags)
			command.SetOut(output)
			command.SetErr(output)
			command.SilenceUsage = true
			command.SilenceErrors = true
			err := command.ExecuteContext(t.Context())
			refused := err != nil && strings.Contains(err.Error(), embeddedBackfillRefusal)
			if refused != testCase.wantRefusal {
				t.Fatalf("error = %v, want refusal %t", err, testCase.wantRefusal)
			}
			if testCase.wantRefusal {
				for _, want := range []string{"ReprojectScalars", "append-only"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q does not mention %s", err.Error(), want)
					}
				}
				if _, statErr := os.Stat(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "clyde")); !os.IsNotExist(statErr) {
					t.Fatalf("refused command wrote the conversation cache: %v", statErr)
				}
			}
		})
	}
}
