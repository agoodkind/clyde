package daemon_test

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/checkpointtest"
)

const (
	checkpointsSourceID = "checkpoints-single"
	checkpointsTargetID = "checkpoints-shared"
)

// checkpointsRoots contains temporary XDG directories for the built CLI.
type checkpointsRoots struct {
	home   string
	config string
	state  string
}

func (roots checkpointsRoots) checkpointDir(collectionID string) string {
	return filepath.Join(roots.state, "clyde", "conversation-semantic", "checkpoints", vectorsearch.CollectionName(collectionID))
}

func (roots checkpointsRoots) writeConfig(t *testing.T, ingestionEnabled bool) {
	t.Helper()
	directory := filepath.Join(roots.config, "clyde")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	content := []byte("[conversation.semantic]\nbackend = \"milvus\"\nsearch_enabled = true\ningestion_enabled = " + strconv.FormatBool(ingestionEnabled) + "\n")
	if err := os.WriteFile(filepath.Join(directory, "config.toml"), content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// writeRecord writes a fingerprint record in the daemon's JSON format under
// fileName in the collection's record directory in the temporary state directory.
func (roots checkpointsRoots) writeRecord(t *testing.T, collectionID string, fileName string, conversationID string, fingerprint string) {
	t.Helper()
	checkpointtest.WriteRecord(t, roots.checkpointDir(collectionID), fileName, conversationID, fingerprint)
}

func (roots checkpointsRoots) run(t *testing.T, binary string) (string, string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, "conversation", "index", "copy-shared",
		"--from", checkpointsSourceID, "--to", checkpointsTargetID, "--checkpoints-only")
	command.Env = append(os.Environ(), "HOME="+roots.home, "XDG_CONFIG_HOME="+roots.config, "XDG_STATE_HOME="+roots.state, "XDG_CACHE_HOME="+filepath.Join(roots.home, "cache"))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func buildCheckpointsCLI(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("go.mod not found above the working directory")
		}
		directory = parent
	}
	binary := filepath.Join(t.TempDir(), "clyde")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/clyde")
	command.Dir = directory
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		t.Fatalf("build clyde CLI: %v\n%s", buildErr, output)
	}
	return binary
}

// TestCopySharedCheckpointsOnlyCopiesSavedRecords builds clyde and runs
// copy-shared --checkpoints-only with temporary XDG directories. It checks that
// the command refuses to run with ingestion enabled without creating the target
// directory. With ingestion disabled, it checks the printed record count and
// verifies that both collections store exactly the source records.
func TestCopySharedCheckpointsOnlyCopiesSavedRecords(t *testing.T) {
	binary := buildCheckpointsCLI(t)
	home := t.TempDir()
	roots := checkpointsRoots{home: home, config: filepath.Join(home, "config"), state: filepath.Join(home, "state")}
	roots.writeRecord(t, checkpointsSourceID, "one.json", "codex:one", "saved-one")
	roots.writeRecord(t, checkpointsSourceID, "two.json", "codex:two", "saved-two")
	want := map[string]string{"codex:one": "saved-one", "codex:two": "saved-two"}

	roots.writeConfig(t, true)
	if stdout, stderr, err := roots.run(t, binary); err == nil {
		t.Fatalf("copy-shared --checkpoints-only ran while ingestion was enabled\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if _, err := os.Stat(roots.checkpointDir(checkpointsTargetID)); !os.IsNotExist(err) {
		t.Fatalf("refused run created %s", roots.checkpointDir(checkpointsTargetID))
	}

	roots.writeConfig(t, false)
	stdout, stderr, err := roots.run(t, binary)
	if err != nil {
		t.Fatalf("copy-shared --checkpoints-only failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if want := "Copied 2 conversation fingerprint records."; !strings.Contains(stdout, want) {
		t.Fatalf("copy-shared printed %q; expected it to contain %q", stdout, want)
	}
	if got := checkpointtest.Records(t, roots.checkpointDir(checkpointsTargetID)); !maps.Equal(got, want) {
		t.Fatalf("target stores records %v, want %v", got, want)
	}
	if got := checkpointtest.Records(t, roots.checkpointDir(checkpointsSourceID)); !maps.Equal(got, want) {
		t.Fatalf("source stores records %v after the copy, want %v", got, want)
	}
}
