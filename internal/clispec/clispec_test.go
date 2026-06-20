package clispec

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
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

func TestCLISinkRawBytesSkipsMetadata(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	sink := NewCLISink(ctx, &out)
	if err := sink.RawBytes([]byte("body")); err != nil {
		t.Fatalf("RawBytes: %v", err)
	}
	if got := out.String(); got != "body" {
		t.Errorf("raw bytes output: got %q, want %q", got, "body")
	}
}

func TestMCPSinkCollects(t *testing.T) {
	t.Parallel()
	sink := &MCPSink{}
	_ = sink.Text("a")
	_ = sink.Bytes([]byte("b"))
	_ = sink.RawBytes([]byte("c"))
	_ = sink.WriteFile("ignored", []byte("d"))
	_ = sink.Copy(context.Background(), []byte("e"))
	if got := sink.String(); got != "abcde" {
		t.Errorf("String(): got %q, want %q", got, "abcde")
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

type probePayload struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
	On    bool   `json:"on"`
	Mode  string `json:"mode"`
}

func (probeInput) isClispecInput()               {}
func (probeInput) isClispecPrepared()            {}
func (probePayload) isClispecStructuredPayload() {}

// probeOp uses P = probeInput with an identity Prepare: the test exercises the
// binding and rendering machinery, not input validation, so the prepared payload
// is the bound input unchanged.
func probeOp() Operation[probeInput, probeInput] {
	return Operation[probeInput, probeInput]{
		Name:       Name{Canonical: "probe_op"},
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "probe",
		Args: []Arg[probeInput]{
			PositionalArg("the_id", "id", func(in *probeInput, v string) { in.ID = v }),
		},
		Params: []Param[probeInput]{
			IntParam("count", "count", 7, func(in *probeInput, v int) { in.Count = v }),
			BoolParam("on", "on", false, func(in *probeInput, v bool) { in.On = v }),
			EnumParam("mode", "mode", "alpha", []string{"alpha", "beta"}, func(in *probeInput, v string) { in.Mode = v }),
		},
		New:      func() probeInput { return probeInput{ID: "", Count: 7, On: false, Mode: "alpha", Surface: SurfaceCLI} },
		Children: nil,
		Prepare:  func(in probeInput) (probeInput, error) { return in, nil },
		Run:      nil,
		runResult: func(_ context.Context, in probeInput) (Result, error) {
			payload := probePayload{ID: in.ID, Count: in.Count, On: in.On, Mode: in.Mode}
			return valueResult{Payload: payload, Text: formatProbe(payload)}, nil
		},
	}
}

func formatProbe(in probePayload) string {
	onText := "off"
	if in.On {
		onText = "on"
	}
	return in.ID + ":" + itoa(in.Count) + ":" + onText + ":" + in.Mode
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

func rootWithChild(child *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "root"}
	output.PersistentFlag(root)
	root.AddCommand(child)
	return root
}

func TestCobraRenderBindsAllKinds(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(probeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"probe-op", "abc", "--count", "12", "--on", "--mode", "beta"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != "abc:12:on:beta" {
		t.Errorf("cli output: got %q, want %q", got, "abc:12:on:beta")
	}
}

func TestCobraRenderUsesDefaults(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(probeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"probe-op", "abc"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != "abc:7:off:alpha" {
		t.Errorf("cli output: got %q, want %q", got, "abc:7:off:alpha")
	}
}

func TestCobraRenderJSONUsesStructuredPayload(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(probeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"--output-format", "json", "probe-op", "abc"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if _, ok := got["_meta"]; !ok {
		t.Fatalf("json output missing _meta: %s", out.String())
	}
}

type artifactProbePayload struct {
	Text string `json:"text"`
}

func (artifactProbePayload) isClispecStructuredPayload() {}

func artifactProbeOp() Operation[probeInput, probeInput] {
	return Operation[probeInput, probeInput]{
		Name:       Name{Canonical: "artifact_probe"},
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindArtifact,
		Short:      "artifact probe",
		Args:       nil,
		Params:     nil,
		New:        func() probeInput { return probeInput{} },
		Children:   nil,
		Prepare:    func(in probeInput) (probeInput, error) { return in, nil },
		Run:        nil,
		runResult: func(_ context.Context, _ probeInput) (Result, error) {
			return artifactResult{
				Payload:     artifactProbePayload{Text: "json-text"},
				Body:        []byte("body"),
				DefaultPath: "",
				Pipe:        false,
				Copy:        false,
				Text:        "human-text",
				InlineText:  "inline-text",
			}, nil
		},
	}
}

func TestCobraRenderJSONUsesStructuredPayloadForInlineArtifact(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(artifactProbeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"--output-format", "json", "artifact-probe"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if _, ok := got["_meta"]; !ok {
		t.Fatalf("json output missing _meta: %s", out.String())
	}
}

func TestCobraRejectsUnknownEnum(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(probeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"probe-op", "abc", "--mode", "gamma"})
	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.Execute(); err == nil {
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
	if text != "xyz:3:on:alpha" {
		t.Errorf("mcp output: got %q, want %q", text, "xyz:3:on:alpha")
	}
}

func TestMCPHandlerStructuredContent(t *testing.T) {
	t.Parallel()
	_, handler := probeOp().mcpTool()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"the_id": "xyz"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.StructuredContent == nil {
		t.Fatal("structured content missing")
	}
	raw, ok := result.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("structured content type = %T, want json.RawMessage", result.StructuredContent)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal structured content: %v\n%s", err, string(raw))
	}
	if _, ok := got["_meta"]; !ok {
		t.Fatalf("structured content missing _meta: %s", string(raw))
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
	want := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\nxyz:7:off:alpha"
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

func taskOnlyProbeOp() Operation[probeInput, probeInput] {
	return Operation[probeInput, probeInput]{
		Name:           Name{Canonical: "task_only_probe"},
		Surfaces:       SurfaceSet{CLI: false, MCP: true},
		outputKind:     resultKindValue,
		Short:          "task-only probe",
		Args:           nil,
		Params:         nil,
		New:            func() probeInput { return probeInput{} },
		Children:       nil,
		Prepare:        func(in probeInput) (probeInput, error) { return in, nil },
		Run:            nil,
		runResult:      nil,
		MCPTaskSupport: mcp.TaskSupportOptional,
		MCPTaskRun:     nil,
		mcpTaskResult: func(_ context.Context, _ probeInput) (Result, error) {
			return valueResult{
				Payload: probePayload{ID: "task", Count: 1, On: false, Mode: "alpha"},
				Text:    "task-only",
			}, nil
		},
	}
}

func TestMCPResultHandlerRejectsTaskOnlyOperationWithoutTask(t *testing.T) {
	t.Parallel()
	_, handler := taskOnlyProbeOp().mcpTool()
	result, err := handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := textOf(t, result); got != "task_only_probe requires task-augmented MCP calls" {
		t.Fatalf("task-only error: got %q, want %q", got, "task_only_probe requires task-augmented MCP calls")
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
