package clispec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/mark3labs/mcp-go/mcp"

	"goodkind.io/clyde/internal/cli/clipboard"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/response"
)

type resultKind uint8

const (
	resultKindValue resultKind = iota + 1
	resultKindArtifact
)

func (kind resultKind) String() string {
	switch kind {
	case resultKindValue:
		return "value"
	case resultKindArtifact:
		return "artifact"
	default:
		return "unknown"
	}
}

// StructuredPayload is the closed JSON payload contract for clispec outputs.
type StructuredPayload interface {
	isClispecStructuredPayload()
}

// Result is the closed operation result contract.
type Result interface {
	isClispecResult()
	kind() resultKind
}

type valueResult struct {
	Payload StructuredPayload
	Text    string
}

type artifactResult struct {
	Payload     StructuredPayload
	Body        []byte
	DefaultPath string
	Pipe        bool
	Text        string
	InlineText  string
}

func (valueResult) isClispecResult()    {}
func (artifactResult) isClispecResult() {}

func (valueResult) kind() resultKind    { return resultKindValue }
func (artifactResult) kind() resultKind { return resultKindArtifact }

func renderCLIResult(ctx context.Context, out io.Writer, errOut io.Writer, format output.Format, wantKind resultKind, result Result) error {
	if err := requireResultKind(wantKind, result); err != nil {
		return err
	}
	if err := renderCLIResultBody(ctx, out, format, result); err != nil {
		return err
	}
	if copyRequested(ctx) {
		return copyResultToClipboard(ctx, errOut, format, result)
	}
	return nil
}

// renderCLIResultBody writes the result to the terminal in the chosen format,
// exactly as it would without --copy. The global --copy flag is layered on top
// by renderCLIResult after this returns, so copy is additive and never replaces
// normal stdout or file output.
func renderCLIResultBody(ctx context.Context, writer io.Writer, format output.Format, result Result) error {
	switch typed := result.(type) {
	case valueResult:
		if format == output.FormatJSON {
			return writeStructuredJSON(ctx, writer, typed.Payload)
		}
		if err := response.WriteText(ctx, writer, typed.Text); err != nil {
			slog.WarnContext(ctx, "clispec.result.write_text_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
			return fmt.Errorf("write value result text: %w", err)
		}
		return nil
	case artifactResult:
		return renderCLIArtifactResult(ctx, writer, format, typed)
	default:
		return fmt.Errorf("clispec: unsupported cli result %T", result)
	}
}

func renderCLIArtifactResult(
	ctx context.Context,
	writer io.Writer,
	format output.Format,
	result artifactResult,
) error {
	if result.Pipe {
		return writeRawBytes(writer, result.Body)
	}
	path := result.DefaultPath
	if path == "" {
		return renderCLIInlineArtifactResult(ctx, writer, format, result)
	}
	if err := writeFile(path, result.Body); err != nil {
		return err
	}
	if format == output.FormatJSON {
		return writeStructuredJSON(ctx, writer, result.Payload)
	}
	if err := response.WriteText(ctx, writer, result.Text); err != nil {
		slog.WarnContext(ctx, "clispec.result.write_artifact_text_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("write artifact result text: %w", err)
	}
	return nil
}

// clipboardCopy is the seam for the platform clipboard. Tests swap it so they
// never touch the real pasteboard.
var clipboardCopy = clipboard.Copy

// copyResultToClipboard honors the global --copy flag. It is layered on top of
// normal output, so the terminal still writes the result to stdout or a file;
// copy is additive, never a replacement. The copied bytes match the selected
// format, so --format json copies the JSON a reader would see. The line-count
// confirmation goes to errOut (stderr) so it never corrupts piped stdout data.
// This is the single place copy is applied, so no command implements copy.
func copyResultToClipboard(ctx context.Context, errOut io.Writer, format output.Format, result Result) error {
	body, err := resultCopyBytes(ctx, format, result)
	if err != nil {
		return err
	}
	if err := clipboardCopy(ctx, body); err != nil {
		slog.WarnContext(ctx, "clispec.result.copy_failed", "concern", "cli.output", "component", "clispec", "err", err)
		return fmt.Errorf("copy output to clipboard: %w", err)
	}
	count := copyLineCount(body)
	unit := "lines"
	if count == 1 {
		unit = "line"
	}
	if _, err := fmt.Fprintf(errOut, "copied %d %s\n", count, unit); err != nil {
		slog.WarnContext(ctx, "clispec.result.copy_confirm_failed", "concern", "cli.output", "component", "clispec", "err", err)
		return fmt.Errorf("write copy confirmation: %w", err)
	}
	return nil
}

// resultCopyBytes returns the bytes to copy for a result in the selected format.
// JSON copies the same structured document the terminal renders; every other
// format copies the raw content body (the artifact body or the value text), not
// the trace-id metadata header.
func resultCopyBytes(ctx context.Context, format output.Format, result Result) ([]byte, error) {
	if format == output.FormatJSON {
		var buf bytes.Buffer
		if err := writeStructuredJSON(ctx, &buf, resultStructuredPayload(result)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	switch typed := result.(type) {
	case artifactResult:
		return typed.Body, nil
	case valueResult:
		return []byte(typed.Text), nil
	default:
		return nil, fmt.Errorf("clispec: unsupported cli result %T for copy", result)
	}
}

// resultStructuredPayload returns the structured payload of a result for JSON
// copying, or nil when the result carries none.
func resultStructuredPayload(result Result) StructuredPayload {
	switch typed := result.(type) {
	case artifactResult:
		return typed.Payload
	case valueResult:
		return typed.Payload
	default:
		return nil
	}
}

// copyLineCount counts lines in body, treating a missing trailing newline as a
// final line. Empty body has zero lines.
func copyLineCount(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	count := bytes.Count(body, []byte("\n"))
	if body[len(body)-1] != '\n' {
		count++
	}
	return count
}

func renderCLIInlineArtifactResult(
	ctx context.Context,
	writer io.Writer,
	format output.Format,
	result artifactResult,
) error {
	if format == output.FormatJSON {
		return writeStructuredJSON(ctx, writer, result.Payload)
	}
	if err := response.WriteText(ctx, writer, result.Text); err != nil {
		slog.WarnContext(ctx, "clispec.result.write_inline_artifact_text_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("write inline artifact text: %w", err)
	}
	return nil
}

func renderMCPResult(ctx context.Context, wantKind resultKind, result Result) (*mcp.CallToolResult, error) {
	if err := requireResultKind(wantKind, result); err != nil {
		return nil, err
	}
	switch typed := result.(type) {
	case valueResult:
		return newMCPStructuredResult(ctx, typed.Payload, typed.Text)
	case artifactResult:
		text := typed.InlineText
		if text == "" {
			text = string(typed.Body)
		}
		return newMCPStructuredResult(ctx, typed.Payload, text)
	default:
		return nil, fmt.Errorf("clispec: unsupported mcp result %T", result)
	}
}

func requireResultKind(wantKind resultKind, result Result) error {
	if result == nil {
		return fmt.Errorf("clispec: nil result for %s output", wantKind)
	}
	if result.kind() != wantKind {
		return fmt.Errorf("clispec: operation declared %s output but returned %s", wantKind, result.kind())
	}
	return nil
}

func writeStructuredJSON(ctx context.Context, writer io.Writer, payload StructuredPayload) error {
	body, err := marshalStructuredPayload(payload)
	if err != nil {
		return err
	}
	if err := response.WriteJSON(ctx, writer, body, response.JSONCompact); err != nil {
		slog.WarnContext(ctx, "clispec.result.write_json_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("write structured json: %w", err)
	}
	return nil
}

func marshalStructuredPayload(payload StructuredPayload) ([]byte, error) {
	if payload == nil {
		slog.Warn("clispec.result.payload_missing", "concern", "cli.conversation", "component", "clispec")
		return nil, fmt.Errorf("clispec: structured payload is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("clispec.result.payload_marshal_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return nil, fmt.Errorf("marshal clispec payload: %w", err)
	}
	return body, nil
}

func newMCPStructuredResult(ctx context.Context, payload StructuredPayload, text string) (*mcp.CallToolResult, error) {
	body, err := marshalStructuredPayload(payload)
	if err != nil {
		return nil, err
	}
	encoded, err := response.JSON(ctx, body, response.JSONCompact)
	if err != nil {
		slog.WarnContext(ctx, "clispec.result.encode_structured_response_failed", "concern", "mcp.server.context", "component", "clispec", "err", err)
		return nil, fmt.Errorf("encode structured response: %w", err)
	}
	return mcp.NewToolResultStructured(json.RawMessage(encoded), response.Text(ctx, text)), nil
}

func writeRawBytes(writer io.Writer, body []byte) error {
	if _, err := writer.Write(body); err != nil {
		slog.Warn("clispec.result.raw_write_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("clispec: write raw bytes: %w", err)
	}
	return nil
}

func writeFile(path string, body []byte) error {
	slog.Debug("clispec.result.write_file", "concern", "cli.conversation", "component", "clispec", "path", path, "bytes", len(body))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		slog.Warn("clispec.result.write_file_failed", "concern", "cli.conversation", "component", "clispec", "path", path, "err", err)
		return fmt.Errorf("clispec: write file %s: %w", path, err)
	}
	return nil
}
