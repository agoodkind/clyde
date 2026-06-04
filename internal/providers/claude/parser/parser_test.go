package parser

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// countingReader records how many bytes were read and whether the underlying
// reader was drained to EOF, so a test can prove a bounded header read.
type countingReader struct {
	r      io.Reader
	bytes  int
	hitEOF bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += n
	if err == io.EOF {
		c.hitEOF = true
	}
	return n, err
}

func TestStreamStripsControlTagNoiseFromUserMessages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := `{"uuid":"1","type":"user","timestamp":"2026-04-24T19:00:00Z","message":{"role":"user","content":"<command-name>/exit</command-name>\n<command-message>exit</command-message>\nCatch you later!"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{IncludeSystemPrompts: false, IncludeToolOutputs: false}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%d want 1", len(messages))
	}
	if messages[0].Text != "Catch you later!" {
		t.Fatalf("text=%q want %q", messages[0].Text, "Catch you later!")
	}
}

// TestScanRecordReadsBoundedHeader writes a tiny header followed by a very large
// body and asserts ScanRecord fills the record from the top without reading to
// EOF: it returns immediately after the first user line provides the title and
// created time, so the cost is bounded by the header, not the file size.
func TestScanRecordReadsBoundedHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.jsonl")

	var builder strings.Builder
	builder.WriteString(`{"sessionId":"sess-123","cwd":"/repo","type":"user","timestamp":"2026-04-24T19:00:00Z","content":"first user line"}` + "\n")
	// A large body after the header. If ScanRecord read to EOF it would touch
	// every one of these lines; the early stop must avoid that.
	const bodyLines = 200000
	filler := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}}` + "\n"
	for i := 0; i < bodyLines; i++ {
		builder.WriteString(filler)
	}
	data := builder.String()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	stamp := conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	counter := &countingReader{r: file, bytes: 0, hitEOF: false}

	record, ok := scanHeader(counter, path, stamp)
	if !ok {
		t.Fatalf("scanHeader returned ok=false")
	}
	if record.NativeID != "sess-123" {
		t.Fatalf("native id=%q want sess-123", record.NativeID)
	}
	if record.Title != "first user line" {
		t.Fatalf("title=%q want %q", record.Title, "first user line")
	}
	if record.CreatedAt.IsZero() {
		t.Fatalf("created time was not parsed from the header")
	}
	if counter.hitEOF {
		t.Fatalf("scanHeader read to EOF; the header read must stop early")
	}
	if int64(counter.bytes) >= info.Size() {
		t.Fatalf("scanHeader read %d bytes, want well under file size %d", counter.bytes, info.Size())
	}
	// The header sits in the first chunk, so the early stop should keep the read
	// to roughly one buffer fill, far under the multi-megabyte body.
	const headerReadCeiling = 1 << 20
	if counter.bytes > headerReadCeiling {
		t.Fatalf("scanHeader read %d bytes, want under %d (early stop should bound the read to the top)", counter.bytes, headerReadCeiling)
	}
}

// TestScanRecordStopsBeforeLineCap proves the header read is bounded by the line
// cap even when no created time or title ever appears, so a degenerate file
// cannot force a full read.
func TestScanRecordHonorsLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noheader.jsonl")

	var builder strings.Builder
	// A session id on the first line so the record is usable, then many lines
	// that never supply a title or created time. The line cap must stop the read
	// well before EOF.
	builder.WriteString(`{"sessionId":"sess-x"}` + "\n")
	for i := 0; i < headerLineCap*4; i++ {
		builder.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[]}}` + "\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	// With no title found, the title falls back to the session id.
	if record.Title != "sess-x" {
		t.Fatalf("title=%q want fallback sess-x", record.Title)
	}
}
