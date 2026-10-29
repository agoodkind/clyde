package localbackendcli_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const (
	localProofTraceMarker     = "🔎"
	localProofSearchMarker    = "🔍"
	localProofTraceIDField    = "trace_id="
	localProofCollectionLabel = "clyde-conversations"
)

// assertLocalProofPublicText searches with the real clyde binary in JSON format and selects the first matching conversation ID.
// It checks clyde conversation search text and clyde_search MCP text for that ID, and clyde daemon status text for backend=local.
// It rejects U+1F50D, U+1F50E, trace_id=, and clyde-conversations in all three outputs.
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
	for _, marker := range []string{localProofTraceMarker, localProofSearchMarker, localProofTraceIDField, localProofCollectionLabel} {
		if strings.Contains(text, marker) {
			t.Errorf("%s contains a magnifier character, trace annotation, or collection label: %s", surface, text)
			return
		}
	}
}
