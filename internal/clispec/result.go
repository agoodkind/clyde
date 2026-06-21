package clispec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/mark3labs/mcp-go/mcp"

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

type textResultPayload struct {
	Text string `json:"text"`
}

type artifactResult struct {
	Payload     StructuredPayload
	Body        []byte
	DefaultPath string
	Pipe        bool
	Copy        bool
	Text        string
	InlineText  string
}

func (valueResult) isClispecResult()                  {}
func (artifactResult) isClispecResult()               {}
func (textResultPayload) isClispecStructuredPayload() {}

func (valueResult) kind() resultKind    { return resultKindValue }
func (artifactResult) kind() resultKind { return resultKindArtifact }

func renderCLIResult(ctx context.Context, writer io.Writer, format output.Format, wantKind resultKind, result Result) error {
	if err := requireResultKind(wantKind, result); err != nil {
		return err
	}
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
	if result.Copy {
		return renderCLIArtifactCopyResult(ctx, writer, format, result)
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

func renderCLIArtifactCopyResult(
	ctx context.Context,
	writer io.Writer,
	format output.Format,
	result artifactResult,
) error {
	if err := copyToClipboard(ctx, result.Body); err != nil {
		return err
	}
	if format == output.FormatJSON {
		return writeStructuredJSON(ctx, writer, result.Payload)
	}
	if err := response.WriteText(ctx, writer, result.Text); err != nil {
		slog.WarnContext(ctx, "clispec.result.write_copy_text_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("write copy result text: %w", err)
	}
	return nil
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

func copyToClipboard(ctx context.Context, body []byte) error {
	if runtime.GOOS != "darwin" {
		slog.WarnContext(ctx, "clispec.result.copy_unsupported_platform", "concern", "cli.conversation", "component", "clispec", "goos", runtime.GOOS)
		return fmt.Errorf("--copy requires macOS pbcopy; use --stdout or --output on this platform")
	}
	cmd := exec.CommandContext(ctx, "pbcopy")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.WarnContext(ctx, "clispec.result.copy_stdin_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("open pbcopy stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		slog.WarnContext(ctx, "clispec.result.copy_start_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("start pbcopy: %w", err)
	}
	if _, err := stdin.Write(body); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		slog.WarnContext(ctx, "clispec.result.copy_write_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("write pbcopy stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		slog.WarnContext(ctx, "clispec.result.copy_close_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("close pbcopy stdin: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		slog.WarnContext(ctx, "clispec.result.copy_wait_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("finish pbcopy: %w", err)
	}
	return nil
}
