package daemon

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/sandbox"
)

func TestWriteSandboxBannerUsesConversationBrowseCommand(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: &output}}
	roots := sandbox.Roots{
		Base:    "/tmp/clyde-sandbox-test",
		State:   "/tmp/clyde-sandbox-test/state",
		Config:  "/tmp/clyde-sandbox-test/config",
		Cache:   "/tmp/clyde-sandbox-test/cache",
		Runtime: "/tmp/clyde-sandbox-test/run",
	}

	writeSandboxBanner(factory, roots, "/tmp/clyde-sandbox-test/config/clyde/config.toml", false, false)

	want := sandbox.ExportLine(roots) + " clyde conversation search"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("sandbox banner missing browse command %q:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), "clyde conversation list") {
		t.Fatalf("sandbox banner contains nonexistent command:\n%s", output.String())
	}
}

func TestSandboxConfigDirectionsAreIndependent(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name             string
		ingestionEnabled bool
		searchEnabled    bool
	}{
		{name: "both disabled", ingestionEnabled: false, searchEnabled: false},
		{name: "ingestion only", ingestionEnabled: true, searchEnabled: false},
		{name: "search only", ingestionEnabled: false, searchEnabled: true},
		{name: "both enabled", ingestionEnabled: true, searchEnabled: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(sandboxConfigTemplate, testCase.ingestionEnabled, testCase.searchEnabled, sandboxCollectionID)
			var cfg config.Config
			if err := toml.Unmarshal([]byte(body), &cfg); err != nil {
				t.Fatalf("parse sandbox config: %v", err)
			}
			if got := cfg.Conversation.Semantic.FeedsEngine(); got != testCase.ingestionEnabled {
				t.Fatalf("FeedsEngine() = %v, want %v", got, testCase.ingestionEnabled)
			}
			if got := cfg.Conversation.Semantic.AnswersSearch(); got != testCase.searchEnabled {
				t.Fatalf("AnswersSearch() = %v, want %v", got, testCase.searchEnabled)
			}
		})
	}
}

func TestSandboxSemanticFlagsDefaultOff(t *testing.T) {
	t.Parallel()

	cmd := newSandboxCmd(&cli.Factory{})
	for _, name := range []string{"ingestion-enabled", "search-enabled"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("sandbox flag --%s is missing", name)
		}
		if flag.DefValue != "false" {
			t.Fatalf("sandbox flag --%s default = %q, want false", name, flag.DefValue)
		}
	}
}
