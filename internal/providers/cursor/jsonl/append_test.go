package cursorjsonl

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type countingTranscriptReader struct {
	*strings.Reader
	bytesRead int
}

func (reader *countingTranscriptReader) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	reader.bytesRead += count
	return count, err
}

func TestScanAppendReadsOnlyNewCompleteRecords(t *testing.T) {
	first := `{"role":"user","message":{"content":[{"type":"text","text":"original title"}]}}` + "\n"
	second := `{"role":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}` + "\n"
	partial := `{"role":"user","message":{"content":[{"type":"text","text":"next"}]}}`
	reader := &countingTranscriptReader{Reader: strings.NewReader(first + second + partial)}
	var messages []TranscriptMessage
	collect := func(message TranscriptMessage) { messages = append(messages, message) }
	header, offset, err := ScanAppend(t.Context(), "chat.jsonl", reader, 0, TranscriptHeader{}, collect)
	if err != nil || offset != int64(len(first+second)) || len(messages) != 2 || header.FirstUserText != "original title" {
		t.Fatalf("first scan: header=%+v offset=%d messages=%+v err=%v", header, offset, messages, err)
	}
	if reader.bytesRead != len(first+second+partial) {
		t.Fatalf("initial bytes = %d", reader.bytesRead)
	}
	reader = &countingTranscriptReader{Reader: strings.NewReader(first + second + partial + "\n")}
	messages = nil
	header, offset, err = ScanAppend(t.Context(), "chat.jsonl", reader, offset, header, collect)
	if err != nil || offset != int64(len(first+second+partial)+1) || len(messages) != 1 || header.FirstUserText != "original title" {
		t.Fatalf("append: header=%+v offset=%d messages=%+v err=%v", header, offset, messages, err)
	}
	if reader.bytesRead != len(partial)+1 {
		t.Fatalf("append read %d bytes, want %d with no prefix reread", reader.bytesRead, len(partial)+1)
	}
	t.Logf("initial=%d bytes, append=%d bytes, skipped prefix=%d bytes", len(first+second+partial), reader.bytesRead, len(first+second))
}

func TestScanAppendStopsAtCancellationBoundary(t *testing.T) {
	body := `{"role":"user","message":{"content":[{"type":"text","text":"one"}]}}` + "\n"
	ctx, cancel := context.WithCancel(t.Context())
	reader := &countingTranscriptReader{Reader: strings.NewReader(strings.Repeat(body, 1000))}
	count := 0
	_, offset, err := ScanAppend(ctx, "chat.jsonl", reader, 0, TranscriptHeader{}, func(TranscriptMessage) {
		count++
		cancel()
	})
	if !errors.Is(err, context.Canceled) || count != 1 || offset != int64(len(body)) {
		t.Fatalf("count=%d offset=%d err=%v", count, offset, err)
	}
	if reader.bytesRead >= len(body)*1000 {
		t.Fatal("cancellation read the remainder")
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
}
