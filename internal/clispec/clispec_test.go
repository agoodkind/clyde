package clispec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/gklog/correlation"
)

func TestNameSpellings(t *testing.T) {
	t.Parallel()
	name := Name{Canonical: "get_conversation"}
	if got := name.CLI(); got != "get-conversation" {
		t.Errorf("CLI(): got %q, want %q", got, "get-conversation")
	}
	if got := name.MCP(); got != "clyde_get_conversation" {
		t.Errorf("MCP(): got %q, want %q", got, "clyde_get_conversation")
	}
}

func TestCLISinkWriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	sink := NewCLISink(context.Background(), &bytes.Buffer{})
	if err := sink.WriteFile(path, []byte("body")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "body" {
		t.Errorf("file body: got %q, want %q", got, "body")
	}
}

func TestMCPSinkCollects(t *testing.T) {
	t.Parallel()
	sink := &MCPSink{}
	_ = sink.Text("a")
	_ = sink.Bytes([]byte("b"))
	_ = sink.WriteFile("ignored", []byte("c"))
	if got := sink.String(); got != "abc" {
		t.Errorf("String(): got %q, want %q", got, "abc")
	}
	if sink.Surface() != SurfaceMCP {
		t.Errorf("Surface(): got %d, want SurfaceMCP", sink.Surface())
	}
}

// probeInput exercises every parameter kind plus a positional argument.
type probeInput struct {
	ID      string
	Count   int
	On      bool
	Mode    string
	Surface Surface
}

func (probeInput) isClispecInput() {}

func probeOp() Operation[probeInput] {
	return Operation[probeInput]{
		Name:     Name{Canonical: "probe_op"},
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "probe",
		Args: []Arg[probeInput]{
			PositionalArg("the_id", "id", func(in *probeInput, v string) { in.ID = v }),
		},
		Params: []Param[probeInput]{
			IntParam("count", "count", 7, func(in *probeInput, v int) { in.Count = v }),
			BoolParam("on", "on", false, func(in *probeInput, v bool) { in.On = v }),
			EnumParam("mode", "mode", "alpha", []string{"alpha", "beta"}, func(in *probeInput, v string) { in.Mode = v }),
		},
		New: func() probeInput { return probeInput{Count: 7, Mode: "alpha"} },
		Run: func(_ context.Context, in probeInput, surface Surface, sink ResultSink) error {
			in.Surface = surface
			return sink.Text(formatProbe(in))
		},
	}
}

func formatProbe(in probeInput) string {
	onText := "off"
	if in.On {
		onText = "on"
	}
	surfaceText := "cli"
	if in.Surface == SurfaceMCP {
		surfaceText = "mcp"
	}
	return in.ID + ":" + itoa(in.Count) + ":" + onText + ":" + in.Mode + ":" + surfaceText
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func testFactory(out *bytes.Buffer) *cli.Factory {
	return &cli.Factory{
		IOStreams: &cli.IOStreams{In: &bytes.Buffer{}, Out: out, Err: &bytes.Buffer{}},
	}
}

func TestCobraRenderBindsAllKinds(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := probeOp().cobraCommand(testFactory(&out))
	cmd.SetArgs([]string{"abc", "--count", "12", "--on", "--mode", "beta"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != "abc:12:on:beta:cli" {
		t.Errorf("cli output: got %q, want %q", got, "abc:12:on:beta:cli")
	}
}

func TestCobraRenderUsesDefaults(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := probeOp().cobraCommand(testFactory(&out))
	cmd.SetArgs([]string{"abc"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != "abc:7:off:alpha:cli" {
		t.Errorf("cli output: got %q, want %q", got, "abc:7:off:alpha:cli")
	}
}

func TestCobraRejectsUnknownEnum(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cmd := probeOp().cobraCommand(testFactory(&out))
	cmd.SetArgs([]string{"abc", "--mode", "gamma"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for unknown enum value, got nil")
	}
}

func TestMCPHandlerBindsAndIsLenient(t *testing.T) {
	t.Parallel()
	_, handler := probeOp().mcpTool()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"the_id": "xyz",
		"count":  float64(3),
		"on":     true,
		"mode":   "unknown_falls_back",
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := textOf(t, result)
	if text != "xyz:3:on:alpha:mcp" {
		t.Errorf("mcp output: got %q, want %q", text, "xyz:3:on:alpha:mcp")
	}
}

func TestMCPHandlerPrependsCorrelationMetadata(t *testing.T) {
	t.Parallel()
	_, handler := probeOp().mcpTool()
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"the_id": "xyz",
	}
	result, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	want := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\nxyz:7:off:alpha:mcp"
	if got := textOf(t, result); got != want {
		t.Fatalf("mcp output: got %q, want %q", got, want)
	}
}

func TestMCPHandlerRequiresPositional(t *testing.T) {
	t.Parallel()
	_, handler := probeOp().mcpTool()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := textOf(t, result); got != "the_id is required" {
		t.Errorf("missing positional: got %q, want %q", got, "the_id is required")
	}
}

func TestDefaultExportOutputPath(t *testing.T) {
	t.Parallel()
	got := defaultExportOutputPath("claude:1a2b/3c", "json")
	if got != "claude-1a2b-3c.json" {
		t.Fatalf("defaultExportOutputPath() = %q, want %q", got, "claude-1a2b-3c.json")
	}
}

func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("nil result")
	}
	if len(result.Content) != 1 {
		t.Fatalf("content length: got %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type: got %T, want mcp.TextContent", result.Content[0])
	}
	return text.Text
}
