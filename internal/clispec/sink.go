package clispec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"goodkind.io/clyde/internal/response"
)

// ResultSink is where a work function writes its result without knowing which
// front end called it. The terminal sink writes to the screen and can write a
// file; the MCP sink collects text in memory and hands it back to the calling
// program.
type ResultSink interface {
	// Text writes the primary human-readable result.
	Text(s string) error
	// Bytes writes raw bytes, such as an export body.
	Bytes(b []byte) error
	// RawBytes writes exact bytes with no metadata or response envelope.
	RawBytes(b []byte) error
	// WriteFile writes bytes to a path on the terminal. The MCP sink has no
	// file contract, so it folds the bytes into its in-memory buffer.
	WriteFile(path string, b []byte) error
	// Copy writes bytes to the system clipboard when the terminal supports it.
	Copy(ctx context.Context, b []byte) error
	// Surface reports the calling front end so a work function can pick a
	// rendering variant where the two front ends differ.
	Surface() Surface
}

// CLISink writes to the terminal output stream.
type CLISink struct {
	metadata response.Metadata
	out      io.Writer
	wrote    bool
}

// NewCLISink builds a terminal sink over the given output stream.
func NewCLISink(ctx context.Context, out io.Writer) *CLISink {
	return &CLISink{metadata: response.FromContext(ctx), out: out, wrote: false}
}

// Text writes a text response to the terminal stream.
func (s *CLISink) Text(text string) error {
	if s.wrote {
		if _, err := io.WriteString(s.out, text); err != nil {
			slog.Warn("clispec.sink.write_text_continuation_failed", "concern", "cli.conversation", "component", "cli", "err", err)
			return fmt.Errorf("clispec: write text continuation: %w", err)
		}
		return nil
	}
	s.wrote = true
	if _, err := io.WriteString(s.out, s.metadata.Text(text)); err != nil {
		slog.Warn("clispec.sink.write_text_response_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write text response: %w", err)
	}
	return nil
}

// Bytes writes raw bytes to the terminal stream after the response metadata line.
func (s *CLISink) Bytes(body []byte) error {
	if !s.wrote {
		header := s.metadata.HeaderLine()
		if header != "" {
			if _, err := io.WriteString(s.out, header+"\n"); err != nil {
				slog.Warn("clispec.sink.write_byte_metadata_failed", "concern", "cli.conversation", "component", "cli", "err", err)
				return fmt.Errorf("clispec: write byte response metadata: %w", err)
			}
		}
		s.wrote = true
	}
	if _, err := s.out.Write(body); err != nil {
		slog.Warn("clispec.sink.write_byte_response_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write byte response: %w", err)
	}
	return nil
}

// RawBytes writes exact bytes to the terminal stream without metadata.
func (s *CLISink) RawBytes(body []byte) error {
	s.wrote = true
	if _, err := s.out.Write(body); err != nil {
		slog.Warn("clispec.sink.write_raw_byte_response_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write raw byte response: %w", err)
	}
	return nil
}

// WriteFile writes the bytes to the path with owner-only permissions and logs
// the outcome.
func (s *CLISink) WriteFile(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o600); err != nil {
		slog.Warn("clispec.sink.write_file_failed", "concern", "cli.conversation", "component", "cli", "path", path, "err", err)
		return fmt.Errorf("clispec: write file %s: %w", path, err)
	}
	slog.Debug("clispec.sink.write_file", "concern", "cli.conversation", "component", "cli", "path", path, "bytes", len(body))
	return nil
}

// Copy writes the bytes to pbcopy on macOS.
func (s *CLISink) Copy(ctx context.Context, body []byte) error {
	if runtime.GOOS != "darwin" {
		slog.WarnContext(ctx, "clispec.sink.copy_unsupported_platform", "concern", "cli.conversation", "component", "cli", "goos", runtime.GOOS)
		return fmt.Errorf("--copy requires macOS pbcopy; use --stdout or --output on this platform")
	}
	cmd := exec.CommandContext(ctx, "pbcopy")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.WarnContext(ctx, "clispec.sink.copy_stdin_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("open pbcopy stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		slog.WarnContext(ctx, "clispec.sink.copy_start_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("start pbcopy: %w", err)
	}
	if _, err := stdin.Write(body); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		slog.WarnContext(ctx, "clispec.sink.copy_write_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("write pbcopy stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		slog.WarnContext(ctx, "clispec.sink.copy_close_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("close pbcopy stdin: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		slog.WarnContext(ctx, "clispec.sink.copy_wait_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("finish pbcopy: %w", err)
	}
	slog.DebugContext(ctx, "clispec.sink.copy", "concern", "cli.conversation", "component", "cli", "bytes", len(body))
	return nil
}

// Surface reports SurfaceCLI.
func (s *CLISink) Surface() Surface {
	return SurfaceCLI
}

// MCPSink collects text in memory for return as one MCP tool result.
type MCPSink struct {
	buf strings.Builder
}

// Text appends the string to the buffer.
func (s *MCPSink) Text(text string) error {
	s.buf.WriteString(text)
	return nil
}

// Bytes appends the bytes to the buffer.
func (s *MCPSink) Bytes(body []byte) error {
	s.buf.Write(body)
	return nil
}

// RawBytes appends the bytes to the buffer.
func (s *MCPSink) RawBytes(body []byte) error {
	s.buf.Write(body)
	return nil
}

// WriteFile folds the bytes into the buffer. The MCP tool has no file
// contract, so a work function never reaches this on the MCP surface; the
// method exists to satisfy ResultSink.
func (s *MCPSink) WriteFile(_ string, body []byte) error {
	s.buf.Write(body)
	return nil
}

// Copy folds the bytes into the buffer. The MCP surface has no clipboard
// contract, so a work function should only call this from CLI-only paths.
func (s *MCPSink) Copy(_ context.Context, body []byte) error {
	s.buf.Write(body)
	return nil
}

// Surface reports SurfaceMCP.
func (s *MCPSink) Surface() Surface {
	return SurfaceMCP
}

// String returns the collected text.
func (s *MCPSink) String() string {
	return s.buf.String()
}
