package clispec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	// RawBytes writes exact bytes without adding metadata to the body.
	RawBytes(b []byte) error
	// WriteFile writes bytes to a path on the terminal. The MCP sink has no
	// file contract, so it folds the bytes into its in-memory buffer.
	WriteFile(path string, b []byte) error
	// Surface reports the calling front end so a work function can pick a
	// rendering variant where the two front ends differ.
	Surface() Surface
}

// CLISink writes to the terminal output stream.
type CLISink struct {
	metadata      response.Metadata
	out           io.Writer
	errOut        io.Writer
	headerWritten bool
}

// NewCLISink builds a terminal sink that writes result bodies to out and
// routes the correlation header to errOut, so the trace-id metadata never
// mixes into piped stdout data.
func NewCLISink(ctx context.Context, out io.Writer, errOut io.Writer) *CLISink {
	return &CLISink{
		metadata:      response.FromContext(ctx),
		out:           out,
		errOut:        errOut,
		headerWritten: false,
	}
}

// Text writes a text response to the terminal stream.
func (s *CLISink) Text(text string) error {
	header, body := response.SplitHeader(text)
	if err := s.writeHeader(header); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, body); err != nil {
		slog.Warn("clispec.sink.write_text_response_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write text response: %w", err)
	}
	return nil
}

// Bytes writes raw bytes to stdout after routing metadata to stderr.
func (s *CLISink) Bytes(body []byte) error {
	if err := s.writeHeader(""); err != nil {
		return err
	}
	if _, err := s.out.Write(body); err != nil {
		slog.Warn("clispec.sink.write_byte_response_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write byte response: %w", err)
	}
	return nil
}

// RawBytes writes exact bytes to stdout after routing metadata to stderr.
func (s *CLISink) RawBytes(body []byte) error {
	if err := s.writeHeader(""); err != nil {
		return err
	}
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

// Surface reports SurfaceCLI.
func (s *CLISink) Surface() Surface {
	return SurfaceCLI
}

func (s *CLISink) writeHeader(stampedHeader string) error {
	if s.headerWritten {
		return nil
	}
	header := s.metadata.HeaderLine()
	if header != "" {
		if _, err := io.WriteString(s.errOut, header+"\n"); err != nil {
			slog.Warn("clispec.sink.write_header_failed", "concern", "cli.conversation", "component", "cli", "err", err)
			return fmt.Errorf("clispec: write response metadata: %w", err)
		}
		s.headerWritten = true
		return nil
	}
	if stampedHeader == "" {
		return nil
	}
	if _, err := io.WriteString(s.errOut, stampedHeader+"\n"); err != nil {
		slog.Warn("clispec.sink.write_stamped_header_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("clispec: write stamped response metadata: %w", err)
	}
	s.headerWritten = true
	return nil
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

// Surface reports SurfaceMCP.
func (s *MCPSink) Surface() Surface {
	return SurfaceMCP
}

// String returns the collected text.
func (s *MCPSink) String() string {
	return s.buf.String()
}
