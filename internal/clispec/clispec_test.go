package clispec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	conv "goodkind.io/clyde/internal/conversation"
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
	sink := NewCLISink(context.Background(), &bytes.Buffer{}, &bytes.Buffer{})
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
	var errOut bytes.Buffer
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	sink := NewCLISink(ctx, &out, &errOut)
	if err := sink.RawBytes([]byte("body")); err != nil {
		t.Fatalf("RawBytes: %v", err)
	}
	if got := out.String(); got != "body" {
		t.Errorf("raw bytes output: got %q, want %q", got, "body")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Errorf("raw bytes metadata: got %q, want %q", got, wantHeader)
	}
}

func TestCLISinkTextWritesMetadataToErrOut(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var errOut bytes.Buffer
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	sink := NewCLISink(ctx, &out, &errOut)
	if err := sink.Text("body"); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if got := out.String(); got != "body" {
		t.Errorf("text output: got %q, want %q", got, "body")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Errorf("text metadata: got %q, want %q", got, wantHeader)
	}
}

func TestCLISinkBytesWritesMetadataToErrOut(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var errOut bytes.Buffer
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	sink := NewCLISink(ctx, &out, &errOut)
	if err := sink.Bytes([]byte("body")); err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if got := out.String(); got != "body" {
		t.Errorf("byte output: got %q, want %q", got, "body")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Errorf("byte metadata: got %q, want %q", got, wantHeader)
	}
}

func TestMCPSinkCollects(t *testing.T) {
	t.Parallel()
	sink := &MCPSink{}
	_ = sink.Text("a")
	_ = sink.Bytes([]byte("b"))
	_ = sink.RawBytes([]byte("c"))
	_ = sink.WriteFile("ignored", []byte("d"))
	if got := sink.String(); got != "abcd" {
		t.Errorf("String(): got %q, want %q", got, "abcd")
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

type stringListProbeInput struct {
	Items []string
}

type stringListProbePayload struct {
	Items []string `json:"items"`
}

func (stringListProbeInput) isClispecInput()               {}
func (stringListProbeInput) isClispecPrepared()            {}
func (stringListProbePayload) isClispecStructuredPayload() {}

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

func stringListProbeOp() Operation[stringListProbeInput, stringListProbeInput] {
	return Operation[stringListProbeInput, stringListProbeInput]{
		Name:       Name{Canonical: "string_list_probe"},
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "string-list probe",
		Args:       nil,
		Params: []Param[stringListProbeInput]{
			StringSliceParam("items", "items", nil, func(in *stringListProbeInput, v []string) { in.Items = append([]string(nil), v...) }),
		},
		New:      func() stringListProbeInput { return stringListProbeInput{Items: nil} },
		Children: nil,
		Prepare:  func(in stringListProbeInput) (stringListProbeInput, error) { return in, nil },
		Run:      nil,
		runResult: func(_ context.Context, in stringListProbeInput) (Result, error) {
			return valueResult{
				Payload: stringListProbePayload{Items: append([]string(nil), in.Items...)},
				Text:    strings.Join(in.Items, ","),
			}, nil
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

func TestCobraRenderBindsStringSliceParam(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(stringListProbeOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"string-list-probe", "--items", "alpha,beta", "--items", "gamma"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != "alpha,beta,gamma" {
		t.Fatalf("string list cli output: got %q, want %q", got, "alpha,beta,gamma")
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

func TestCopyLineCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "trailing newline", body: "a\nb\nc\n", want: 3},
		{name: "no trailing newline", body: "a\nb\nc", want: 3},
		{name: "empty", body: "", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := copyLineCount([]byte(tc.body)); got != tc.want {
				t.Errorf("copyLineCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRenderCLIResultTextWritesMetadataToErrOut(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var errOut bytes.Buffer
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	result := valueResult{
		Payload: probePayload{ID: "trace", Count: 1, On: false, Mode: "alpha"},
		Text:    "value text",
	}

	err := renderCLIResult(ctx, &out, &errOut, output.FormatText, resultKindValue, result)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}

	if got := out.String(); got != "value text" {
		t.Fatalf("stdout = %q, want %q", got, "value text")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Fatalf("stderr = %q, want %q", got, wantHeader)
	}
}

func TestRenderCLIResultTextRoutesStampedHeaderToErrOut(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var errOut bytes.Buffer
	result := valueResult{
		Payload: probePayload{ID: "trace", Count: 1, On: false, Mode: "alpha"},
		Text:    "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\nvalue text",
	}

	err := renderCLIResult(context.Background(), &out, &errOut, output.FormatText, resultKindValue, result)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}

	if got := out.String(); got != "value text" {
		t.Fatalf("stdout = %q, want %q", got, "value text")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Fatalf("stderr = %q, want %q", got, wantHeader)
	}
}

func TestRenderCLIResultPipeWritesMetadataToErrOut(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var errOut bytes.Buffer
	body := []byte("raw body\n")
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	result := artifactResult{
		Payload:     artifactProbePayload{Text: "json-text"},
		Body:        body,
		DefaultPath: "",
		Pipe:        true,
		Text:        "",
		InlineText:  string(body),
	}

	err := renderCLIResult(ctx, &out, &errOut, output.FormatText, resultKindArtifact, result)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}

	if got := out.String(); got != string(body) {
		t.Fatalf("stdout = %q, want %q", got, string(body))
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Fatalf("stderr = %q, want %q", got, wantHeader)
	}
}

// TestRenderCopyResultCopiesArtifactBody asserts --copy is additive: the normal
// output still renders to stdout, the artifact body is copied, and the line-count
// confirmation lands on stderr, not stdout.
func TestRenderCopyResultCopiesArtifactBody(t *testing.T) {
	body := []byte("alpha\nbeta\n")
	var copied []byte
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, got []byte) error {
		copied = append([]byte(nil), got...)
		return nil
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := artifactResult{
		Payload:     artifactProbePayload{Text: "json-text"},
		Body:        body,
		DefaultPath: "",
		Pipe:        false,
		Text:        "human-text",
		InlineText:  "inline-text",
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatText,
		resultKindArtifact,
		result,
	)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	if !bytes.Equal(copied, body) {
		t.Fatalf("copied body = %q, want %q", string(copied), string(body))
	}
	if got := out.String(); got != "human-text" {
		t.Fatalf("normal output = %q, want %q", got, "human-text")
	}
	if got := errOut.String(); got != "copied 2 lines\n" {
		t.Fatalf("copy confirmation = %q, want %q", got, "copied 2 lines\n")
	}
}

// TestRenderJSONResultWritesNoHeaderToErrOut asserts JSON output keeps its
// metadata inside the document as _meta and does not also emit the text
// trace-id header on stderr, so JSON behavior is unchanged.
func TestRenderJSONResultWritesNoHeaderToErrOut(t *testing.T) {
	t.Parallel()
	ctx := correlation.WithContext(context.Background(), correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	})
	var out, errOut bytes.Buffer
	result := valueResult{
		Payload: probePayload{ID: "json", Count: 1, On: false, Mode: "alpha"},
		Text:    "value text",
	}
	if err := renderCLIResult(ctx, &out, &errOut, output.FormatJSON, resultKindValue, result); err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty (JSON keeps _meta in the document, no stderr header)", got)
	}
	if !strings.Contains(out.String(), "_meta") {
		t.Fatalf("stdout = %q, want JSON containing _meta", out.String())
	}
}

func TestRenderCopyOnlyArtifactWritesNoFile(t *testing.T) {
	var copied []byte
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, got []byte) error {
		copied = append([]byte(nil), got...)
		return nil
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := artifactResult{
		Payload:     artifactProbePayload{Text: "json-text"},
		Body:        []byte("a\nb\n"),
		DefaultPath: "",
		Pipe:        false,
		Text:        "",
		InlineText:  "",
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatText,
		resultKindArtifact,
		result,
	)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if !bytes.Equal(copied, []byte("a\nb\n")) {
		t.Fatalf("copied body = %q, want %q", string(copied), "a\nb\n")
	}
	if got := errOut.String(); got != "copied 2 lines\n" {
		t.Fatalf("copy confirmation = %q, want %q", got, "copied 2 lines\n")
	}
}

// TestRenderCopyResultCopiesValueText asserts the value text is copied and the
// singular "line" form is used for a one-line body.
func TestRenderCopyResultCopiesValueText(t *testing.T) {
	var copied []byte
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, got []byte) error {
		copied = append([]byte(nil), got...)
		return nil
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := valueResult{
		Payload: probePayload{ID: "copy", Count: 2, On: false, Mode: "alpha"},
		Text:    "value text",
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatText,
		resultKindValue,
		result,
	)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	if got := string(copied); got != "value text" {
		t.Fatalf("copied value = %q, want %q", got, "value text")
	}
	if got := out.String(); got != "value text" {
		t.Fatalf("normal output = %q, want %q", got, "value text")
	}
	if got := errOut.String(); got != "copied 1 line\n" {
		t.Fatalf("copy confirmation = %q, want %q", got, "copied 1 line\n")
	}
}

// TestRenderCopyResultJSONCopiesJSON asserts --format json copies the JSON
// document a reader would see, not the plain text body, and that the copied
// bytes match what the terminal rendered.
func TestRenderCopyResultJSONCopiesJSON(t *testing.T) {
	var copied []byte
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, got []byte) error {
		copied = append([]byte(nil), got...)
		return nil
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := valueResult{
		Payload: probePayload{ID: "copy", Count: 2, On: true, Mode: "beta"},
		Text:    "value text",
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatJSON,
		resultKindValue,
		result,
	)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	var copiedDoc map[string]json.RawMessage
	if err := json.Unmarshal(copied, &copiedDoc); err != nil {
		t.Fatalf("copied bytes are not JSON: %v\n%s", err, string(copied))
	}
	if string(copied) == "value text" {
		t.Fatal("copied the text body, want the JSON document")
	}
	if got := out.String(); got != string(copied) {
		t.Fatalf("copied JSON %q does not match rendered stdout %q", string(copied), got)
	}
}

// TestRenderCopyResultAdditiveWithStdout asserts copy and --stdout do both: the
// raw body streams to stdout uncorrupted, the body is copied, and the
// confirmation is isolated on stderr.
func TestRenderCopyResultAdditiveWithStdout(t *testing.T) {
	body := []byte("line1\nline2\nline3\n")
	var copied []byte
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, got []byte) error {
		copied = append([]byte(nil), got...)
		return nil
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := artifactResult{
		Payload:     artifactProbePayload{Text: "json-text"},
		Body:        body,
		DefaultPath: "",
		Pipe:        true,
		Text:        "",
		InlineText:  string(body),
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatText,
		resultKindArtifact,
		result,
	)
	if err != nil {
		t.Fatalf("renderCLIResult: %v", err)
	}
	if got := out.String(); got != string(body) {
		t.Fatalf("stdout = %q, want raw body %q", got, string(body))
	}
	if !bytes.Equal(copied, body) {
		t.Fatalf("copied body = %q, want %q", string(copied), string(body))
	}
	if got := errOut.String(); got != "copied 3 lines\n" {
		t.Fatalf("copy confirmation = %q, want %q", got, "copied 3 lines\n")
	}
}

// TestRenderCopyResultSurfacesClipboardError asserts a clipboard failure (as on
// non-macOS platforms) surfaces as an error after the body was written, with no
// false "copied" confirmation.
func TestRenderCopyResultSurfacesClipboardError(t *testing.T) {
	originalClipboardCopy := clipboardCopy
	clipboardCopy = func(_ context.Context, _ []byte) error {
		return errors.New("clipboard copy is not supported on this platform")
	}
	t.Cleanup(func() { clipboardCopy = originalClipboardCopy })

	var out, errOut bytes.Buffer
	result := artifactResult{
		Payload:     artifactProbePayload{Text: "json-text"},
		Body:        []byte("data\n"),
		DefaultPath: "",
		Pipe:        true,
		Text:        "",
		InlineText:  "data\n",
	}

	err := renderCLIResult(
		withCopy(context.Background(), true),
		&out,
		&errOut,
		output.FormatText,
		resultKindArtifact,
		result,
	)
	if err == nil {
		t.Fatal("expected clipboard error to surface")
	}
	if got := out.String(); got != "data\n" {
		t.Fatalf("stdout = %q, want body written before copy failure", got)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("no confirmation should print on copy failure, got %q", got)
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

func TestMitmBaselineSeedCommandRejectsMissingUpstream(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := rootWithChild(mitmBaselineSeedOp().cobraCommand(testFactory(&out)))
	root.SetArgs([]string{"seed"})
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing upstream")
	} else if !strings.Contains(err.Error(), "upstream is required") {
		t.Fatalf("missing upstream error = %q, want substring %q", err.Error(), "upstream is required")
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

func TestRenderMCPArtifactWritesFileAndPreservesResult(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "claude-probe.txt")
	body := []byte("export body\n")
	result, err := renderMCPResult(context.Background(), resultKindArtifact, artifactResult{
		Payload: exportTranscriptOutput{
			ConversationID: "claude:probe",
			Format:         "plain_text",
			Path:           path,
			Bytes:          len(body),
		},
		Body:        body,
		DefaultPath: path,
		InlineText:  string(body),
	})
	if err != nil {
		t.Fatalf("renderMCPResult: %v", err)
	}

	gotBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("export file body = %q, want %q", string(gotBody), string(body))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat export file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("export file permissions = %o, want 600", got)
	}
	if got := textOf(t, result); got != string(body) {
		t.Fatalf("MCP text = %q, want %q", got, string(body))
	}

	raw, ok := result.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("structured content type = %T, want json.RawMessage", result.StructuredContent)
	}
	var got exportTranscriptOutput
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal structured content: %v\n%s", err, string(raw))
	}
	if got.ConversationID != "claude:probe" || got.Format != "plain_text" || got.Path != path || got.Bytes != len(body) {
		t.Fatalf("structured export = %#v, want preserved export metadata", got)
	}

	replacement := []byte("replacement export body\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("make export file permissive: %v", err)
	}
	_, err = renderMCPResult(context.Background(), resultKindArtifact, artifactResult{
		Payload: exportTranscriptOutput{
			ConversationID: "claude:probe",
			Format:         "plain_text",
			Path:           path,
			Bytes:          len(replacement),
		},
		Body:        replacement,
		DefaultPath: path,
		InlineText:  string(replacement),
	})
	if err != nil {
		t.Fatalf("render replacement MCP result: %v", err)
	}
	gotBody, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replacement export file: %v", err)
	}
	if !bytes.Equal(gotBody, replacement) {
		t.Fatalf("replacement export file body = %q, want %q", string(gotBody), string(replacement))
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat replacement export file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replacement export file permissions = %o, want 600", got)
	}
}

func TestRenderMCPArtifactReturnsWriteFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "export.txt")
	_, err := renderMCPResult(context.Background(), resultKindArtifact, artifactResult{
		Payload:     artifactProbePayload{Text: "metadata"},
		Body:        []byte("body"),
		DefaultPath: path,
		InlineText:  "body",
	})
	if err == nil {
		t.Fatal("renderMCPResult succeeded when the artifact file could not be written")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("write error = %q, want path %q", err, path)
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

func TestMCPHandlerBindsStringSliceParam(t *testing.T) {
	t.Parallel()
	_, handler := stringListProbeOp().mcpTool()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": []any{"alpha", "beta"}}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := textOf(t, result); got != "alpha,beta" {
		t.Fatalf("string list mcp output: got %q, want %q", got, "alpha,beta")
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

func TestExportDestinationPath(t *testing.T) {
	t.Parallel()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	defaultPath := defaultExportOutputPath("claude:probe", conv.ExportFormatMarkdown)
	cases := []struct {
		name       string
		outputPath string
		stdout     bool
		copy       bool
		want       string
	}{
		{name: "implicit file", outputPath: "", stdout: false, copy: false, want: filepath.Join(workingDirectory, defaultPath)},
		{name: "explicit file", outputPath: "transcript.md", stdout: false, copy: false, want: filepath.Join(workingDirectory, "transcript.md")},
		{name: "explicit file with copy", outputPath: "transcript.md", stdout: false, copy: true, want: filepath.Join(workingDirectory, "transcript.md")},
		{name: "stdout", outputPath: "", stdout: true, copy: false, want: ""},
		{name: "copy only", outputPath: "", stdout: false, copy: true, want: ""},
		{name: "stdout with copy", outputPath: "", stdout: true, copy: true, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.copy {
				ctx = withCopy(ctx, true)
			}
			payload := exportPayload{
				ConversationID: "claude:probe",
				Options: conv.ExportOptions{
					Format: conv.ExportFormatMarkdown,
				},
				OutputPath: tc.outputPath,
				Stdout:     tc.stdout,
			}
			got, err := exportDestinationPath(ctx, payload)
			if err != nil {
				t.Fatalf("exportDestinationPath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("exportDestinationPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMCPCallerWorkingDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	file := filepath.Join(directory, "file.txt")
	if err := os.WriteFile(file, []byte("body"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	spacedDirectory := filepath.Join(directory, "caller cwd ")
	if err := os.Mkdir(spacedDirectory, 0o700); err != nil {
		t.Fatalf("create spaced directory: %v", err)
	}
	_, err := withMCPCallerWorkingDirectory(context.Background(), mcp.CallToolRequest{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing metadata error = %v, want required cwd error", err)
	}
	cases := []struct {
		name string
		meta map[string]any
		want string
		err  string
	}{
		{name: "cwd", meta: map[string]any{"cwd": directory}, want: directory},
		{name: "cwd with trailing space", meta: map[string]any{"cwd": spacedDirectory}, want: spacedDirectory},
		{name: "codex cwd", meta: map[string]any{"codex_cwd": directory}, want: directory},
		{name: "cwd takes precedence", meta: map[string]any{"cwd": directory, "codex_cwd": "/missing"}, want: directory},
		{name: "missing", meta: nil, err: "required"},
		{name: "non string", meta: map[string]any{"cwd": 3}, err: "must be a string"},
		{name: "empty", meta: map[string]any{"cwd": " "}, err: "must not be empty"},
		{name: "relative", meta: map[string]any{"cwd": "relative"}, err: "must be an absolute path"},
		{name: "file", meta: map[string]any{"cwd": file}, err: "is not a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: &mcp.Meta{AdditionalFields: tc.meta}}}
			ctx, err := withMCPCallerWorkingDirectory(context.Background(), req)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("withMCPCallerWorkingDirectory() error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("withMCPCallerWorkingDirectory: %v", err)
			}
			result, ok := mcpCallerWorkingDirectoryResultFromContext(ctx)
			if !ok || result.Directory != tc.want || result.Err != nil {
				t.Fatalf("caller working directory result = %#v, %t, want %q without error", result, ok, tc.want)
			}
		})
	}
}

func TestExportDestinationPathUsesMCPCallerWorkingDirectory(t *testing.T) {
	t.Parallel()
	callerDirectory := t.TempDir()
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: &mcp.Meta{AdditionalFields: map[string]any{"cwd": callerDirectory}}}}
	ctx, err := withMCPCallerWorkingDirectory(context.Background(), req)
	if err != nil {
		t.Fatalf("withMCPCallerWorkingDirectory: %v", err)
	}
	ctx = withSurface(ctx, SurfaceMCP)
	path, err := exportDestinationPath(ctx, exportPayload{
		ConversationID: "claude:probe",
		Options: conv.ExportOptions{
			Format: conv.ExportFormatPlainText,
		},
	})
	if err != nil {
		t.Fatalf("exportDestinationPath: %v", err)
	}
	want := filepath.Join(callerDirectory, "claude-probe.txt")
	if path != want {
		t.Fatalf("MCP export path = %q, want caller path %q", path, want)
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
