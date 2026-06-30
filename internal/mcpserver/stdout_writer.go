package mcpserver

// stdout serialization wrapper.
//
// The upstream mark3labs/mcp-go stdio transport runs handleNotifications on a
// dedicated goroutine and toolCallWorker on a worker pool. Both write
// JSON-RPC frames to the same [os.Stdout]. Some upstream paths
// (RequestSampling, ListRoots, RequestElicitation) write directly to the
// stdio writer without going through writeResponse, so they bypass the
// upstream writeMu. Two concurrent writes can interleave at the byte level,
// and the receiver sees torn JSON. Wrapping stdout in a writer that locks on
// every Write call serializes every byte regardless of the upstream code
// path.
//
// CLYDE-57: Tool-use concurrency API Error 400 on MCP notifications.

import (
	"fmt"
	"io"
	"sync"
)

// mcpStdoutWriter serializes writes to an underlying [io.Writer].
//
// Each Write call takes stdoutMu, writes the bytes once, and releases the
// lock. The mutex is never held across upstream tool work, network I/O, or
// context cancellation. Only the bytes hitting stdout are serialized.
type mcpStdoutWriter struct {
	stdoutMu sync.Mutex
	w        io.Writer
}

// newMCPStdoutWriter wraps w so concurrent Write calls do not interleave.
func newMCPStdoutWriter(w io.Writer) *mcpStdoutWriter {
	return &mcpStdoutWriter{
		stdoutMu: sync.Mutex{},
		w:        w,
	}
}

// Write serializes p to the underlying writer under stdoutMu.
func (m *mcpStdoutWriter) Write(p []byte) (int, error) {
	m.stdoutMu.Lock()
	defer m.stdoutMu.Unlock()
	n, err := m.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("mcpserver: write stdout frame: %w", err)
	}
	return n, nil
}
