package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	clistatus "goodkind.io/clyde/internal/cli/status"
	"goodkind.io/clyde/internal/daemon"
)

const (
	statusProofUser  = "statususer"
	statusProofPass  = "statuspass7741"
	statusProofToken = "statustoken9925"

	statusProofRedactedBaseURL = "http://REDACTED@localhost:1/v1?token=REDACTED"
	statusProofRedactedMilvus  = "https://REDACTED@[::1]:1?token=REDACTED"

	statusProofDefaultCollectionID = "clyde-conversations"
	statusProofDefaultModel        = "nvidia/NV-EmbedCode-7b-v1"
	statusProofDefaultDimension    = int64(4096)
	statusProofIntervalMS          = int64(2000)

	statusProofJSONNull = "null"

	statusProofBlockedDaemonConfig = `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
milvus_address = "https://` + statusProofUser + `:` + statusProofPass + `@[::1]:1?token=` + statusProofToken + `"
embedding_base_url = "http://` + statusProofUser + `:` + statusProofPass + `@localhost:1/v1?token=` + statusProofToken + `"
sync_interval = "2s"
index_refresh_interval = "2s"

[adapter]
enabled = false

[mitm]
enabled_default = false
`
)

type statusProofJSONField struct {
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit"`
}

type statusProofJSONDocument struct {
	Notices  []string                          `json:"notices"`
	Identity map[string]statusProofJSONField   `json:"identity"`
	Counters map[string]statusProofJSONField   `json:"counters"`
	Activity []map[string]statusProofJSONField `json:"activity"`
}

func runStatusProofCommand(t *testing.T, step string, arguments ...string) string {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	factory := cli.NewSystemFactory(cli.BuildInfo{Version: "status-proof", Commit: "", Date: ""})
	factory.IOStreams = &cli.IOStreams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}
	root := &cobra.Command{Use: "clyde", SilenceUsage: true, SilenceErrors: true}
	output.PersistentFlag(root)
	root.AddCommand(clistatus.NewCmd(factory))
	root.SetArgs(append([]string{"status"}, arguments...))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("step=%s operation=run_status_command arguments=%v err=%v stderr=%s", step, arguments, err, stderr.String())
	}
	return stdout.String()
}

func statusProofJSON(t *testing.T, step string) (statusProofJSONDocument, string) {
	t.Helper()
	body := runStatusProofCommand(t, step, "--"+output.FlagName, string(output.FormatJSON))
	var document statusProofJSONDocument
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("step=%s operation=decode_status_json err=%v body=%s", step, err, body)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("step=%s operation=check_status_json_end err=%v trailing=%s body=%s", step, err, trailing, body)
	}
	if strings.Count(strings.TrimRight(body, "\n"), "\n") != 0 {
		t.Errorf("step=%s operation=check_status_json_lines body=%s", step, body)
	}
	return document, body
}

func assertStatusProofJSONValue(t *testing.T, step string, document statusProofJSONDocument, name string, want string) {
	t.Helper()
	field, found := document.Counters[name]
	if !found {
		t.Errorf("step=%s counter=%s found=false", step, name)
		return
	}
	if got := string(field.Value); got != want {
		t.Errorf("step=%s counter=%s value=%s want=%s", step, name, got, want)
	}
}

func assertStatusProofLines(t *testing.T, step string, body string, lines ...string) {
	t.Helper()
	present := make(map[string]bool)
	for line := range strings.SplitSeq(body, "\n") {
		present[line] = true
	}
	for _, line := range lines {
		if !present[line] {
			t.Errorf("step=%s missing_line=%q body=\n%s", step, line, body)
		}
	}
}

func waitForStatusProof(
	t *testing.T,
	step string,
	timeout time.Duration,
	accept func(daemon.SemanticStatus) bool,
) *daemon.RuntimeStatus {
	t.Helper()
	var last daemon.StatusReport
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		last = daemon.InspectStatus(context.Background())
		if last.Runtime != nil && accept(last.Runtime.Semantic) {
			return last.Runtime
		}
		time.Sleep(ingestionProofPollInterval)
	}
	described, err := json.Marshal(last.Runtime)
	if err != nil {
		t.Fatalf("step=%s operation=encode_status err=%v", step, err)
	}
	t.Fatalf("step=%s operation=wait_for_status timeout=%s daemon_error=%q runtime=%s", step, timeout, last.DaemonError, described)
	return nil
}
