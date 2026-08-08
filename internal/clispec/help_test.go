package clispec

import (
	"bytes"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/daemon"
)

// TestLeafHelpDocumentsArgumentsExamplesAndEnum asserts that a rendered leaf
// command documents its positional arguments, prints an example, and shows
// each constrained flag's accepted values and default.
func TestLeafHelpDocumentsArgumentsExamplesAndEnum(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := exportTranscriptOp().cobraCommand(testFactory(&out))

	if !strings.Contains(cmd.Long, "Arguments:") {
		t.Errorf("long help missing Arguments section: %q", cmd.Long)
	}
	if !strings.Contains(cmd.Long, "CONVERSATION_ID") {
		t.Errorf("long help missing positional placeholder: %q", cmd.Long)
	}
	if !strings.Contains(cmd.Example, "clyde conversation export") {
		t.Errorf("example missing from help: %q", cmd.Example)
	}

	usage := cmd.Flags().FlagUsages()
	if !strings.Contains(usage, "markdown|html|json|plain_text") {
		t.Errorf("flag usage missing enum accepted values: %q", usage)
	}
	if !strings.Contains(usage, "(default markdown)") {
		t.Errorf("flag usage missing default: %q", usage)
	}
}

// TestMCPDescriptionCarriesExamples asserts the example command lines reach the
// MCP tool description, which has no separate help screen.
func TestMCPDescriptionCarriesExamples(t *testing.T) {
	t.Parallel()
	tool, _ := exportTranscriptOp().mcpTool()
	if !strings.Contains(tool.Description, "clyde conversation export") {
		t.Errorf("mcp description missing example: %q", tool.Description)
	}
}

func TestConversationHelpIncludesRegisteredProviders(t *testing.T) {
	t.Parallel()

	search := searchOp()
	helpSurfaces := map[string]string{
		"conversation short": conversationGroup.Short,
		"conversation long":  conversationGroup.Long,
		"search short":       search.Short,
		"search long":        search.Long,
	}
	for _, provider := range daemon.ConversationProviders() {
		label := provider.String()
		displayLabel := strings.ToUpper(label[:1]) + label[1:]
		for name, help := range helpSurfaces {
			if !strings.Contains(help, displayLabel) {
				t.Errorf("%s help missing %s: %q", name, displayLabel, help)
			}
		}
	}

	params := searchParams()
	for _, param := range params {
		if param.Canonical != "provider" {
			continue
		}
		for _, provider := range daemon.ConversationProviders() {
			if !strings.Contains(param.Description, provider.String()) {
				t.Errorf("provider help missing %s: %q", provider, param.Description)
			}
		}
		return
	}
	t.Fatal("provider parameter missing")
}
