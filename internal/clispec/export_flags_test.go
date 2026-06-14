package clispec

import (
	"bytes"
	"slices"
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

// TestExportOnlyParamShape asserts the --only selector is a required enum-list
// over the content-kind selector values, and the per-type shortcuts are
// CLI-only boolean flags.
func TestExportOnlyParamShape(t *testing.T) {
	t.Parallel()
	only := exportParam(t, "only")
	if only.Kind != KindEnumList {
		t.Errorf("only kind = %d, want KindEnumList", only.Kind)
	}
	if !only.Required {
		t.Errorf("only should be required")
	}
	if !slices.Equal(only.Values, conv.ContentKindSelectorValues()) {
		t.Errorf("only values = %v, want %v", only.Values, conv.ContentKindSelectorValues())
	}

	stdout := exportParam(t, "stdout")
	if stdout.Kind != KindBool {
		t.Errorf("stdout kind = %d, want KindBool", stdout.Kind)
	}
	if !stdout.CLIOnly {
		t.Errorf("stdout should be CLI-only")
	}

	for _, canonical := range []string{
		"chat", "thinking", "tool_calls", "tool_outputs",
		"system_prompts", "system_messages", "raw_json_metadata", "tools", "all",
		"preserve", "tidy", "compact", "dense",
	} {
		shortcut := exportParam(t, canonical)
		if shortcut.Kind != KindBool {
			t.Errorf("shortcut %q kind = %d, want KindBool", canonical, shortcut.Kind)
		}
		if !shortcut.CLIOnly {
			t.Errorf("shortcut %q should be CLI-only", canonical)
		}
	}
}

// TestExportSelectorsAppendKinds asserts both the --only list and the shortcut
// flags append their selector values, and the union resolves correctly through
// ResolveContentKinds (tools selects summary-only tool lines).
func TestExportSelectorsAppendKinds(t *testing.T) {
	t.Parallel()
	in := exportTranscriptOp().New()
	exportParam(t, "only").bindStrSlice(&in, []string{"chat", "thinking"})
	exportParam(t, "tools").bindBool(&in, true)
	exportParam(t, "system_prompts").bindBool(&in, false) // absent, not appended

	set, err := conv.ResolveContentKinds(in.Kinds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []conv.ContentKind{
		conv.ContentKindChat, conv.ContentKindThinking, conv.ContentKindToolSummaries,
	}
	if got := set.Kinds(); !slices.Equal(got, want) {
		t.Errorf("resolved kinds = %v, want %v", got, want)
	}
	if set.Has(conv.ContentKindSystemPrompts) {
		t.Errorf("system_prompts should be absent")
	}
}

// TestExportEmptySelectionErrors asserts that selecting nothing is an error.
func TestExportEmptySelectionErrors(t *testing.T) {
	t.Parallel()
	in := exportTranscriptOp().New()
	if _, err := conv.ResolveContentKinds(in.Kinds); err == nil {
		t.Fatal("expected an error when no content kind is selected")
	}
}

// TestExportOnlyFlagAndShortcuts asserts the rendered terminal command exposes
// --only as a list flag and the shortcuts as bool flags, the old presence
// switches are gone, and --only rejects an unknown kind.
func TestExportOnlyFlagAndShortcuts(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := exportTranscriptOp().cobraCommand(testFactory(&out))

	only := cmd.Flags().Lookup("only")
	if only == nil {
		t.Fatal("missing --only flag")
	}
	if only.Value.Type() == "bool" {
		t.Errorf("--only should be a list flag, got type %s", only.Value.Type())
	}

	for _, name := range []string{
		"chat", "thinking", "tool-calls", "tool-outputs",
		"system-prompts", "system-messages", "raw-json-metadata", "tools", "all",
		"preserve", "tidy", "compact", "dense",
	} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Errorf("missing shortcut flag --%s", name)
			continue
		}
		if flag.Value.Type() != "bool" {
			t.Errorf("--%s type = %s, want bool", name, flag.Value.Type())
		}
	}

	stdout := cmd.Flags().Lookup("stdout")
	if stdout == nil {
		t.Fatal("missing --stdout flag")
	}
	if stdout.Value.Type() != "bool" {
		t.Errorf("--stdout type = %s, want bool", stdout.Value.Type())
	}

	for _, name := range []string{
		"compaction-scope",
		"compaction-detail",
		"compaction-checkpoint",
	} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Errorf("missing --%s flag", name)
		}
	}

	for _, gone := range []string{"no-thinking", "no-chat", "with-tool-outputs", "include-chat"} {
		if cmd.Flags().Lookup(gone) != nil {
			t.Errorf("old flag --%s should be removed", gone)
		}
	}

	if err := cmd.Flags().Set("only", "bogus"); err == nil {
		t.Error("--only should reject an unknown kind")
	}
}

// TestExportMCPOnlyIsRequiredArray asserts the MCP tool exposes only the `only`
// argument as a required array.
func TestExportMCPOnlyIsRequiredArray(t *testing.T) {
	t.Parallel()
	tool, _ := exportTranscriptOp().mcpTool()

	prop, ok := tool.InputSchema.Properties["only"]
	if !ok {
		t.Fatal("mcp tool missing only property")
	}
	schema, ok := prop.(map[string]any)
	if !ok {
		t.Fatalf("only property is %T, want map", prop)
	}
	if schema["type"] != "array" {
		t.Errorf("only type = %v, want array", schema["type"])
	}
	if !slices.Contains(tool.InputSchema.Required, "only") {
		t.Errorf("only should be required, required = %v", tool.InputSchema.Required)
	}
	for _, name := range []string{"compaction_scope", "compaction_detail", "compaction_checkpoint"} {
		if _, present := tool.InputSchema.Properties[name]; !present {
			t.Errorf("mcp tool missing %s property", name)
		}
	}
	for _, cliOnly := range []string{"chat", "thinking", "tools", "all", "stdout", "preserve", "tidy", "compact", "dense"} {
		if _, present := tool.InputSchema.Properties[cliOnly]; present {
			t.Errorf("cli-only property %q should not appear on the MCP surface", cliOnly)
		}
	}
}

// TestExportMCPDecodeOnly asserts the MCP decode path reads the only array into
// the kind selection.
func TestExportMCPDecodeOnly(t *testing.T) {
	t.Parallel()
	in := exportTranscriptOp().New()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"only": []any{"chat", "tools"}}
	exportParam(t, "only").decodeMCP(&in, req)

	set, err := conv.ResolveContentKinds(in.Kinds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, want := range []conv.ContentKind{conv.ContentKindChat, conv.ContentKindToolSummaries} {
		if !set.Has(want) {
			t.Errorf("decoded set missing %s", want)
		}
	}
}

func TestExportWhitespaceShortcutPrepare(t *testing.T) {
	t.Parallel()
	in := exportTranscriptOp().New()
	in.Kinds = []string{"chat"}
	in.WhitespaceSelections = []conv.WhitespaceMode{conv.WhitespaceDense}
	payload, err := exportTranscriptOp().Prepare(in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if payload.Options.Whitespace != conv.WhitespaceDense {
		t.Fatalf("whitespace = %q, want %q", payload.Options.Whitespace, conv.WhitespaceDense)
	}
}

func TestExportCLIRejectsMultipleWhitespaceSelectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags []string
	}{
		{name: "two shortcuts", flags: []string{"--only", "chat", "--dense", "--compact"}},
		{name: "enum and shortcut", flags: []string{"--only", "chat", "--whitespace", "compact", "--dense"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			cmd := exportTranscriptOp().cobraCommand(testFactory(&out))
			if err := cmd.ParseFlags(tc.flags); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			err := cmd.PreRunE(cmd, []string{"claude:probe"})
			if err == nil {
				t.Fatal("expected whitespace selector conflict")
			}
		})
	}
}

// TestExportPrepareResolvesDestinations asserts the terminal destination flags
// resolve before Run, so the work function can write bytes without parsing flags.
func TestExportPrepareResolvesDestinations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		outputPath     string
		stdout         bool
		wantOutputPath string
		wantStdout     bool
	}{
		{name: "default file", outputPath: "", stdout: false, wantOutputPath: "", wantStdout: false},
		{name: "explicit file", outputPath: "transcript.md", stdout: false, wantOutputPath: "transcript.md", wantStdout: false},
		{name: "stdout flag", outputPath: "", stdout: true, wantOutputPath: "", wantStdout: true},
		{name: "dash output", outputPath: "-", stdout: false, wantOutputPath: "-", wantStdout: true},
		{name: "both stdout spellings", outputPath: "-", stdout: true, wantOutputPath: "-", wantStdout: true},
	}
	op := exportTranscriptOp()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := op.New()
			in.Kinds = []string{"chat"}
			in.OutputPath = tc.outputPath
			in.Stdout = tc.stdout
			payload, err := op.Prepare(in)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if payload.OutputPath != tc.wantOutputPath {
				t.Errorf("OutputPath = %q, want %q", payload.OutputPath, tc.wantOutputPath)
			}
			if payload.Stdout != tc.wantStdout {
				t.Errorf("Stdout = %t, want %t", payload.Stdout, tc.wantStdout)
			}
		})
	}
}

// TestExportPrepareRejectsStdoutFileConflict asserts stdout mode cannot also
// name a regular output file.
func TestExportPrepareRejectsStdoutFileConflict(t *testing.T) {
	t.Parallel()
	in := exportTranscriptOp().New()
	in.Kinds = []string{"chat"}
	in.Stdout = true
	in.OutputPath = "transcript.md"
	_, err := exportTranscriptOp().Prepare(in)
	if err == nil {
		t.Fatal("expected stdout plus output path conflict")
	}
}

// TestExportPrepareCompactionControls asserts compaction controls are normalized
// and rejected before the daemon call.
func TestExportPrepareCompactionControls(t *testing.T) {
	t.Parallel()
	op := exportTranscriptOp()

	currentContext := op.New()
	currentContext.Kinds = []string{"chat"}
	currentContext.Options.Compaction.Scope = conv.CompactionExportScopeCurrentContext
	payload, err := op.Prepare(currentContext)
	if err != nil {
		t.Fatalf("Prepare rejected current_context: %v", err)
	}
	if payload.Options.Compaction.Detail != conv.CompactionExportDetailContext {
		t.Fatalf("detail = %q, want context", payload.Options.Compaction.Detail)
	}

	fromCheckpoint := op.New()
	fromCheckpoint.Kinds = []string{"chat"}
	fromCheckpoint.Options.Compaction.Scope = conv.CompactionExportScopeFromCheckpoint
	if _, err := op.Prepare(fromCheckpoint); err == nil {
		t.Fatal("from_checkpoint without checkpoint should be rejected")
	}

	wrongScope := op.New()
	wrongScope.Kinds = []string{"chat"}
	wrongScope.Options.Compaction.CheckpointNumber = 2
	if _, err := op.Prepare(wrongScope); err == nil {
		t.Fatal("checkpoint with full scope should be rejected")
	}

	historyStart := op.New()
	historyStart.Kinds = []string{"chat"}
	historyStart.Options.HistoryStart = 1
	historyStart.Options.Compaction.Scope = conv.CompactionExportScopeCurrentContext
	if _, err := op.Prepare(historyStart); err == nil {
		t.Fatal("history_start with compaction scope should be rejected")
	}
}
