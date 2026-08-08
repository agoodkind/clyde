package daemon

import (
	"bytes"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
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

	writeSandboxBanner(factory, roots, "/tmp/clyde-sandbox-test/config/clyde/config.toml")

	want := sandbox.ExportLine(roots) + " clyde conversation search"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("sandbox banner missing browse command %q:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), "clyde conversation list") {
		t.Fatalf("sandbox banner contains nonexistent command:\n%s", output.String())
	}
}
