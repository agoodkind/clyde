package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// slowWriter writes one byte at a time to an underlying buffer with no
// synchronization. Without an outer mutex, two concurrent slowWriter.Write
// calls produce interleaved bytes. With mcpStdoutWriter wrapping it, every
// Write call must complete atomically and frames must not interleave.
type slowWriter struct {
	buf *bytes.Buffer
}

// Write deliberately writes bytes one at a time so the runtime preempts
// between bytes and exposes any missing serialization on a wrapping writer.
func (w *slowWriter) Write(p []byte) (int, error) {
	for byteIndex := 0; byteIndex < len(p); byteIndex++ {
		if _, err := w.buf.Write(p[byteIndex : byteIndex+1]); err != nil {
			return byteIndex, err
		}
	}
	return len(p), nil
}

// TestMCPStdoutWriterSerializesFrames spawns several goroutines that each
// write many JSON-RPC frames through mcpStdoutWriter and asserts the result
// parses as a sequence of intact JSON frames. The test is run under -race
// in CI; the deterministic assertion is the JSON parse below.
//
// Reproduces CLYDE-57: handleNotifications and toolCallWorker both write
// JSON-RPC frames to stdout and torn frames cause API Error 400 on the
// receiver.
func TestMCPStdoutWriterSerializesFrames(t *testing.T) {
	const goroutineCount = 16
	const writesPerGoroutine = 200

	var buf bytes.Buffer
	writer := newMCPStdoutWriter(&slowWriter{buf: &buf})

	var wg sync.WaitGroup
	wg.Add(goroutineCount)
	for goroutineIndex := 0; goroutineIndex < goroutineCount; goroutineIndex++ {
		go func(routineID int) {
			defer wg.Done()
			for writeIndex := 0; writeIndex < writesPerGoroutine; writeIndex++ {
				frame := buildFrame(routineID, writeIndex)
				if _, err := writer.Write(frame); err != nil {
					t.Errorf("Write failed: %v", err)
					return
				}
			}
		}(goroutineIndex)
	}
	wg.Wait()

	output := buf.String()
	expectedFrameCount := goroutineCount * writesPerGoroutine
	verifyFrames(t, output, expectedFrameCount)
}

// TestMCPStdoutWriterFrameContentRoundtrips checks that every frame that
// went into the writer comes out exactly as written, with no byte from
// another frame mixed in.
func TestMCPStdoutWriterFrameContentRoundtrips(t *testing.T) {
	const goroutineCount = 8
	const writesPerGoroutine = 150

	var buf bytes.Buffer
	writer := newMCPStdoutWriter(&slowWriter{buf: &buf})

	expected := make(map[string]struct{}, goroutineCount*writesPerGoroutine)
	var expectedMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(goroutineCount)
	for goroutineIndex := 0; goroutineIndex < goroutineCount; goroutineIndex++ {
		go func(routineID int) {
			defer wg.Done()
			for writeIndex := 0; writeIndex < writesPerGoroutine; writeIndex++ {
				frame := buildFrame(routineID, writeIndex)
				expectedMu.Lock()
				expected[strings.TrimRight(string(frame), "\n")] = struct{}{}
				expectedMu.Unlock()
				if _, err := writer.Write(frame); err != nil {
					t.Errorf("Write failed: %v", err)
					return
				}
			}
		}(goroutineIndex)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(expected) {
		t.Fatalf("expected %d frames, got %d", len(expected), len(lines))
	}
	for _, line := range lines {
		if _, ok := expected[line]; !ok {
			t.Fatalf("frame not in expected set, indicates torn write: %q", line)
		}
	}
}

// frame mirrors the JSON-RPC notification shape the upstream stdio
// transport writes through writeResponse and through direct session writes.
type frame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  params `json:"params"`
}

type params struct {
	GoroutineID int `json:"goroutine_id"`
	WriteIndex  int `json:"write_index"`
}

// buildFrame returns a JSON-RPC line frame ending in a newline, matching the
// upstream writeResponse and SetWriter direct-write byte shape.
func buildFrame(goroutineID int, writeIndex int) []byte {
	payload := frame{
		JSONRPC: "2.0",
		Method:  "notifications/test",
		Params: params{
			GoroutineID: goroutineID,
			WriteIndex:  writeIndex,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal frame: %v", err))
	}
	return append(encoded, '\n')
}

// verifyFrames splits the output on newlines, expects expectedCount lines,
// parses each line as a JSON frame, and rejects any malformed line. A torn
// frame would either fail to parse or produce a line count mismatch.
func verifyFrames(t *testing.T, output string, expectedCount int) {
	t.Helper()
	trimmed := strings.TrimRight(output, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) != expectedCount {
		t.Fatalf("expected %d JSON-RPC lines, got %d", expectedCount, len(lines))
	}
	for lineIndex, line := range lines {
		if line == "" {
			t.Fatalf("empty line at index %d, indicates torn frame", lineIndex)
		}
		var decoded frame
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("torn JSON frame at index %d: %v\nline=%q", lineIndex, err, line)
		}
		if decoded.JSONRPC != "2.0" || decoded.Method != "notifications/test" {
			t.Fatalf("unexpected frame contents at index %d: %+v", lineIndex, decoded)
		}
	}
}
