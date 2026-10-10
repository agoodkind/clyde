package localbackendcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clispec"
)

const localBackendSourceJSON = `"source":"local"`

func localBackendSearchCLIOutput(t *testing.T, registry *clispec.Registry, query string) string {
	t.Helper()
	var stdout bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{In: &bytes.Buffer{}, Out: &stdout, Err: &bytes.Buffer{}},
	}
	root := &cobra.Command{Use: "clyde"}
	output.PersistentFlag(root)
	root.AddCommand(clispec.RenderCobra(registry, factory)...)
	root.SetArgs([]string{"--output-format", "json", "conversation", "search", "--query", query})
	if err := root.Execute(); err != nil {
		t.Fatalf("conversation search: %v\n%s", err, stdout.String())
	}
	return stdout.String()
}

func localBackendSearchMCPOutput(t *testing.T, registry *clispec.Registry, query string) string {
	t.Helper()
	mcpServer := server.NewMCPServer("clyde-local-backend-test", "test")
	clispec.RenderMCP(registry, mcpServer)
	request := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"clyde_search","arguments":{"query":%q}}}`,
		query,
	)
	response := mcpServer.HandleMessage(context.Background(), json.RawMessage(request))
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode clyde_search response: %v", err)
	}
	return string(body)
}
