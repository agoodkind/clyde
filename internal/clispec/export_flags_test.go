package clispec

import (
	"bytes"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	conv "goodkind.io/clyde/internal/conversation"
)

// exportParam finds one declared parameter of the export operation by its
// canonical name, failing the test if it is absent.
func exportParam(t *testing.T, canonical string) Param[exportInput] {
	t.Helper()
	for _, p := range exportTranscriptOp().Params {
		if p.Canonical == canonical {
			return p
		}
	}
	t.Fatalf("export op has no parameter %q", canonical)
	return Param[exportInput]{}
}

// exportToggleCases enumerate the seven content-type presence switches, the
// ExportOptions field each governs, and the field value expected when the flag
// is present (v=true) versus absent (v=false). The no_* switches invert; the
// with_* switches pass through.
var exportToggleCases = []struct {
	canonical   string
	read        func(conv.ExportOptions) bool
	wantPresent bool
	wantAbsent  bool
}{
	{"no_chat", func(o conv.ExportOptions) bool { return o.IncludeChat }, false, true},
	{"no_thinking", func(o conv.ExportOptions) bool { return o.IncludeThinking }, false, true},
	{"no_tool_calls", func(o conv.ExportOptions) bool { return o.IncludeToolCalls }, false, true},
	{"with_tool_outputs", func(o conv.ExportOptions) bool { return o.IncludeToolOutputs }, true, false},
	{"with_system_prompts", func(o conv.ExportOptions) bool { return o.IncludeSystemPrompts }, true, false},
	{"with_system_messages", func(o conv.ExportOptions) bool { return o.IncludeSystemMessages }, true, false},
	{"with_raw_json_metadata", func(o conv.ExportOptions) bool { return o.IncludeRawJSONMetadata }, true, false},
}

// TestExportToggleSettersMapBothPolarities asserts each presence switch maps to
// its ExportOptions field correctly in both directions, starting from New().
func TestExportToggleSettersMapBothPolarities(t *testing.T) {
	t.Parallel()
	for _, tc := range exportToggleCases {
		param := exportParam(t, tc.canonical)

		present := exportTranscriptOp().New()
		param.bindBool(&present, true)
		if got := tc.read(present.Options); got != tc.wantPresent {
			t.Errorf("%s present: field = %t, want %t", tc.canonical, got, tc.wantPresent)
		}

		absent := exportTranscriptOp().New()
		param.bindBool(&absent, false)
		if got := tc.read(absent.Options); got != tc.wantAbsent {
			t.Errorf("%s absent: field = %t, want %t", tc.canonical, got, tc.wantAbsent)
		}
	}
}

// TestExportToggleDecodeMCPMatchesCLI asserts the MCP decode path applies the
// same mapping as the CLI binder, so a tool call with no_thinking=true excludes
// thinking and with_tool_outputs=true includes tool outputs.
func TestExportToggleDecodeMCPMatchesCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range exportToggleCases {
		param := exportParam(t, tc.canonical)

		present := exportTranscriptOp().New()
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{tc.canonical: true}
		param.decodeMCP(&present, req)
		if got := tc.read(present.Options); got != tc.wantPresent {
			t.Errorf("%s mcp present: field = %t, want %t", tc.canonical, got, tc.wantPresent)
		}

		absent := exportTranscriptOp().New()
		param.decodeMCP(&absent, mcp.CallToolRequest{})
		if got := tc.read(absent.Options); got != tc.wantAbsent {
			t.Errorf("%s mcp absent: field = %t, want %t", tc.canonical, got, tc.wantAbsent)
		}
	}
}

// TestExportPresenceFlagsReplaceIncludeBools asserts the rendered terminal
// command exposes the seven presence switches as plain bool flags, that the old
// include-* flags are gone, and that a bare presence switch parses without the
// pflag =false form.
func TestExportPresenceFlagsReplaceIncludeBools(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := exportTranscriptOp().cobraCommand(testFactory(&out))

	wantFlags := []string{
		"no-chat", "no-thinking", "no-tool-calls",
		"with-tool-outputs", "with-system-prompts", "with-system-messages", "with-raw-json-metadata",
	}
	for _, name := range wantFlags {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Errorf("missing flag --%s", name)
			continue
		}
		if flag.Value.Type() != "bool" {
			t.Errorf("--%s type = %s, want bool", name, flag.Value.Type())
		}
	}

	for _, gone := range []string{"include-chat", "include-thinking", "include-tool-calls", "include-tool-outputs"} {
		if cmd.Flags().Lookup(gone) != nil {
			t.Errorf("old flag --%s should be removed", gone)
		}
	}

	if err := cmd.Flags().Parse([]string{"--no-thinking", "--with-tool-outputs"}); err != nil {
		t.Fatalf("parse presence flags without =false: %v", err)
	}
}
