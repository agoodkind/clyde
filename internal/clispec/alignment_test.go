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
// The export file-path flag is the only one: the terminal writes a file, the
// MCP tool returns text. A new terminal-only flag must be added here
// deliberately, which keeps the omission visible.
var cliOnlyFlags = map[string]bool{
	"output": true,
}

// TestConversationRegistryNamesAreExactlySix guards the operation set. It
// stands in for the hand-maintained checklist that AGENTS.md used to carry.
func TestConversationRegistryNamesAreExactlySix(t *testing.T) {
	t.Parallel()
	reg := NewConversationRegistry()
	want := []string{
		"clyde_analyze_results",
		"clyde_export_transcript",
		"clyde_get_context",
		"clyde_get_conversation",
		"clyde_list_conversations",
		"clyde_search_conversation",
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

		wantTool := "clyde_" + strings.ReplaceAll(cmd.Name(), "-", "_")
		if tool.Name != wantTool {
			t.Errorf("tool name: got %q, want %q (from command %q)", tool.Name, wantTool, cmd.Name())
		}

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
		// Every positional is a required MCP input.
		required := sliceSet(tool.InputSchema.Required)
		for name := range positionals {
			if !required[name] {
				t.Errorf("%s: positional %q is not required on the mcp surface", tool.Name, name)
			}
		}
	}
}

// TestRenderMCPSkipsCLIOnlyOperation confirms a terminal-only operation
// produces no MCP tool.
func TestRenderMCPSkipsCLIOnlyOperation(t *testing.T) {
	t.Parallel()
	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, Operation[probeInput]{
		Name:     Name{Canonical: "cli_only_probe"},
		Surfaces: SurfaceSet{CLI: true, MCP: false},
		Short:    "cli only",
		Long:     "",
		Args:     nil,
		Params:   nil,
		New:      func() probeInput { return probeInput{ID: "", Count: 0, On: false, Mode: "", Surface: SurfaceCLI} },
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
// arguments from its Use string, e.g. "get-context CONVERSATION_ID".
func positionalNames(cmd *cobra.Command) map[string]bool {
	names := map[string]bool{}
	fields := strings.Fields(cmd.Use)
	for _, field := range fields[1:] {
		names[strings.ToLower(field)] = true
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
