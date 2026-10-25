package localbackendcli_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const (
	localProofTraceMarker  = "🔎"
	localProofTraceIDField = "trace_id="
)

// assertLocalProofPublicText runs one conversation search through the real
// clyde binary in JSON format to find the first matching conversation ID.
// It then checks these public text outputs:
//
//   - The text output of "clyde conversation search" must contain that ID.
//   - The output of "clyde daemon status" must contain "backend=local".
//   - The text content of the clyde_search MCP tool must contain that ID.
//
// Each output must not contain the magnifying glass character or "trace_id=".
func assertLocalProofPublicText(t *testing.T, d *localProofDaemon) {
	t.Helper()
	request := localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofPageLimit, Offset: 0, IncludeArchived: false}
	structured := mustLocalProofSearch(t, localProofSurface{name: "cli", search: d.searchCLI}, request)
	if len(structured.Matches) == 0 {
		t.Fatalf("conversation search returned no matches for %q", request.Query)
	}
	firstConversation := structured.Matches[0].Conversation.ID

	searchText := d.runCLIText(t, "conversation", "search", "--query", request.Query, "--limit", strconv.Itoa(request.Limit))
	assertLocalProofOmitsTrace(t, "conversation search", searchText)
	if !strings.Contains(searchText, firstConversation) {
		t.Errorf("conversation search text does not contain %q:\n%s", firstConversation, searchText)
	}

	statusText := d.status(t)
	assertLocalProofOmitsTrace(t, "daemon status", statusText)
	if !strings.Contains(statusText, "backend=local") {
		t.Errorf("daemon status text does not contain %q:\n%s", "backend=local", statusText)
	}

	response, err := d.callSearchMCP(request)
	if err != nil {
		t.Fatalf("clyde_search: %v", err)
	}
	if response.Result.IsError || len(response.Result.Content) == 0 {
		t.Fatalf("clyde_search returned error=%t with %d content items", response.Result.IsError, len(response.Result.Content))
	}
	for _, content := range response.Result.Content {
		assertLocalProofOmitsTrace(t, "clyde_search", content.Text)
		if !strings.Contains(content.Text, firstConversation) {
			t.Errorf("clyde_search text does not contain %q:\n%s", firstConversation, content.Text)
		}
	}
}

// runCLIText runs the clyde binary with the given arguments and the daemon's
// environment. It returns the combined stdout and stderr text.
// It fails the test when the command fails.
func (d *localProofDaemon) runCLIText(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, d.binaryPath, arguments...)
	command.Env = d.environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("clyde %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func assertLocalProofOmitsTrace(t *testing.T, surface string, text string) {
	t.Helper()
	if strings.Contains(text, localProofTraceMarker) || strings.Contains(text, localProofTraceIDField) {
		t.Errorf("%s text contains a trace annotation:\n%s", surface, text)
	}
}
