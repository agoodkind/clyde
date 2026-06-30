package clispec

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliOnlyFlags lists terminal flags that intentionally have no MCP property.
// A new terminal-only flag must be added here deliberately, which keeps the
// omission visible.
var cliOnlyFlags = map[string]bool{
	"all":    true,
	"copy":   true,
	"output": true,
	"stdout": true,
	// export whitespace shortcut flags: CLI sugar for the shared whitespace enum.
	"preserve": true,
	"tidy":     true,
	"dense":    true,
	// export per-type shortcut flags: CLI sugar for the single MCP `only` array.
	"chat":              true,
	"thinking":          true,
	"tool_calls":        true,
	"tool_outputs":      true,
	"system_prompts":    true,
	"system_messages":   true,
	"raw_json_metadata": true,
	"tools":             true,
}

// TestConversationRegistryNames guards the operation set. It
// stands in for the hand-maintained checklist that AGENTS.md used to carry.
func TestConversationRegistryNames(t *testing.T) {
	t.Parallel()
	reg := NewConversationRegistry()
	want := []string{
		"clyde_conversation_info",
		"clyde_export_transcript",
		"clyde_reorient",
		"clyde_search",
	}
	var got []string
	for _, op := range reg.ops {
		if !op.surfaceSet().MCP {
			continue
		}
		tool, _ := op.mcpTool()
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mcp tool names: got %v, want %v", got, want)
	}
}

// TestConversationAlignment checks, from the single registry, that every
// MCP-exposed operation renders both surfaces and that the inputs match across
// them. A parameter added to one surface alone fails this test.
func TestConversationAlignment(t *testing.T) {
	t.Parallel()
	reg := NewConversationRegistry()
	var out bytes.Buffer
	factory := testFactory(&out)

	for _, op := range reg.ops {
		cmd := op.cobraCommand(factory)
		positionals := positionalNames(cmd)
		flags := flagNames(cmd)

		if !op.surfaceSet().MCP {
			continue
		}
		tool, _ := op.mcpTool()

		props := propertyNames(tool.InputSchema.Properties)
		cliInputs := union(positionals, flags)

		// Every MCP input exists on the terminal too.
		for name := range props {
			if !cliInputs[name] {
				t.Errorf("%s: mcp input %q has no terminal flag or positional", tool.Name, name)
			}
		}
		// Every terminal flag is an MCP input, unless it is a declared
		// terminal-only flag.
		for name := range flags {
			if !props[name] && !cliOnlyFlags[name] {
				t.Errorf("%s: terminal flag %q has no mcp input and is not declared cli-only", tool.Name, name)
			}
		}
		// Every required positional is a required MCP input. Optional
		// positionals still appear on both surfaces but stay optional in MCP.
		required := sliceSet(tool.InputSchema.Required)
		for name, requiredOnCLI := range positionals {
			if requiredOnCLI && !required[name] {
				t.Errorf("%s: positional %q is not required on the mcp surface", tool.Name, name)
			}
			if !requiredOnCLI && required[name] {
				t.Errorf("%s: optional positional %q is required on the mcp surface", tool.Name, name)
			}
		}
	}
}

// TestRenderCobraGroupsConversationOps confirms RenderCobra emits the
// conversation parents at the root with the intended short-verb children.
func TestRenderCobraGroupsConversationOps(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	roots := RenderCobra(NewConversationRegistry(), testFactory(&out))
	if len(roots) != 6 {
		t.Fatalf("root commands: got %d, want 6 (conversation, install, logs, mitm, daemon, mcp)", len(roots))
	}
	parents := map[string]*cobra.Command{}
	for _, root := range roots {
		parents[root.Name()] = root
	}

	conversationParent := parents["conversation"]
	if conversationParent == nil {
		t.Fatal("conversation parent missing")
	}
	wantConversationChildren := []string{"export", "info", "reorient", "search"}
	var gotChildren []string
	for _, child := range conversationParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != strings.Join(wantConversationChildren, ",") {
		t.Fatalf("conversation children: got %v, want %v", gotChildren, wantConversationChildren)
	}

	exportParent, _, err := conversationParent.Find([]string{"export"})
	if err != nil {
		t.Fatalf("find conversation export: %v", err)
	}
	if exportParent == nil {
		t.Fatal("export command missing")
	}
	gotChildren = nil
	for _, child := range exportParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "tail" {
		t.Fatalf("export children: got %v, want [tail]", gotChildren)
	}

	infoLeaf, _, err := conversationParent.Find([]string{"info"})
	if err != nil {
		t.Fatalf("find conversation info: %v", err)
	}
	if infoLeaf == nil {
		t.Fatal("info leaf missing")
	}
	if len(infoLeaf.Commands()) != 0 {
		t.Fatalf("info is a leaf, got %d subcommands", len(infoLeaf.Commands()))
	}

	searchLeaf, _, err := conversationParent.Find([]string{"search"})
	if err != nil {
		t.Fatalf("find conversation search: %v", err)
	}
	if searchLeaf == nil {
		t.Fatal("search leaf missing")
	}
	if len(searchLeaf.Commands()) != 0 {
		t.Fatalf("search is a leaf, got %d subcommands", len(searchLeaf.Commands()))
	}

	logsParent := parents["logs"]
	if logsParent == nil {
		t.Fatal("logs parent missing")
	}
	gotChildren = nil
	for _, child := range logsParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "inventory" {
		t.Fatalf("logs children: got %v, want [inventory]", gotChildren)
	}

	installParent := parents["install"]
	if installParent == nil {
		t.Fatal("install parent missing")
	}
	gotChildren = nil
	for _, child := range installParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "hooks" {
		t.Fatalf("install children: got %v, want [hooks]", gotChildren)
	}

	mitmParent := parents["mitm"]
	if mitmParent == nil {
		t.Fatal("mitm parent missing")
	}
	gotChildren = nil
	for _, child := range mitmParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "baseline,show,status" {
		t.Fatalf("mitm children: got %v, want [baseline show status]", gotChildren)
	}

	baselineParent, _, err := mitmParent.Find([]string{"baseline"})
	if err != nil {
		t.Fatalf("find mitm baseline: %v", err)
	}
	if baselineParent == nil {
		t.Fatal("baseline command missing")
	}
	gotChildren = nil
	for _, child := range baselineParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "seed" {
		t.Fatalf("baseline children: got %v, want [seed]", gotChildren)
	}

	daemonParent := parents["daemon"]
	if daemonParent == nil {
		t.Fatal("daemon parent missing")
	}
	gotChildren = nil
	for _, child := range daemonParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "deploy,fingerprint,reload,run,status,worker" {
		t.Fatalf("daemon children: got %v, want [deploy fingerprint reload run status worker]", gotChildren)
	}

	mcpParent := parents["mcp"]
	if mcpParent == nil {
		t.Fatal("mcp parent missing")
	}
	gotChildren = nil
	for _, child := range mcpParent.Commands() {
		gotChildren = append(gotChildren, child.Name())
	}
	sort.Strings(gotChildren)
	if strings.Join(gotChildren, ",") != "serve" {
		t.Fatalf("mcp children: got %v, want [serve]", gotChildren)
	}
}

// TestRenderMCPSkipsCLIOnlyOperation confirms a terminal-only operation
// produces no MCP tool.
func TestRenderMCPSkipsCLIOnlyOperation(t *testing.T) {
	t.Parallel()
	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, Operation[probeInput, probeInput]{
		Name:     Name{Canonical: "cli_only_probe"},
		Surfaces: SurfaceSet{CLI: true, MCP: false},
		Short:    "cli only",
		Long:     "",
		Args:     nil,
		Params:   nil,
		New:      func() probeInput { return probeInput{ID: "", Count: 0, On: false, Mode: "", Surface: SurfaceCLI} },
		Children: nil,
		Prepare:  func(in probeInput) (probeInput, error) { return in, nil },
		Run: func(_ context.Context, _ probeInput, _ Surface, sink ResultSink) error {
			return sink.Text("x")
		},
	})
	mcpCount := 0
	for _, op := range reg.ops {
		if op.surfaceSet().MCP {
			mcpCount++
		}
	}
	if mcpCount != 0 {
		t.Fatalf("cli-only operation should not be MCP-exposed, got %d mcp ops", mcpCount)
	}
}

// positionalNames extracts the snake_case names of a command's positional
// arguments from its Use string, e.g. "get-context CONVERSATION_ID". The
// returned value is true when the positional is required.
func positionalNames(cmd *cobra.Command) map[string]bool {
	names := map[string]bool{}
	fields := strings.Fields(cmd.Use)
	for _, field := range fields[1:] {
		required := true
		normalized := field
		if strings.HasPrefix(normalized, "[") && strings.HasSuffix(normalized, "]") {
			required = false
			normalized = strings.TrimPrefix(strings.TrimSuffix(normalized, "]"), "[")
		}
		names[strings.ToLower(normalized)] = required
	}
	return names
}

func flagNames(cmd *cobra.Command) map[string]bool {
	names := map[string]bool{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		names[strings.ReplaceAll(f.Name, "-", "_")] = true
	})
	return names
}

func propertyNames(properties map[string]any) map[string]bool {
	names := map[string]bool{}
	for key := range properties {
		names[key] = true
	}
	return names
}

func sliceSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for key := range a {
		out[key] = true
	}
	for key := range b {
		out[key] = true
	}
	return out
}
