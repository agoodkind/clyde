package mitm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const rawCaptureFileMode os.FileMode = 0o600

type cursorCaptureMetadata struct {
	Provider            string
	Host                string
	Path                string
	Method              string
	Status              int
	RequestBytes        int64
	ResponseBytes       int64
	RequestRawPath      string
	ResponseRawPath     string
	RequestID           string
	OriginalRequestID   string
	SessionID           string
	Traceparent         string
	RequestContentType  string
	ResponseContentType string
	Diagnostic          *cursorBidiAppendDiagnostic
}

type cursorCaptureEvent struct {
	Kind                string                      `json:"kind"`
	T                   int64                       `json:"t"`
	TS                  string                      `json:"ts"`
	Provider            string                      `json:"provider"`
	Host                string                      `json:"host"`
	Method              string                      `json:"method"`
	Path                string                      `json:"path"`
	Status              int                         `json:"status"`
	RequestBytes        int64                       `json:"request_bytes"`
	ResponseBytes       int64                       `json:"response_bytes"`
	RequestRawPath      string                      `json:"request_raw_path"`
	ResponseRawPath     string                      `json:"response_raw_path"`
	RequestID           string                      `json:"request_id"`
	OriginalRequestID   string                      `json:"original_request_id"`
	SessionID           string                      `json:"session_id"`
	Traceparent         string                      `json:"traceparent"`
	RequestContentType  string                      `json:"request_content_type"`
	ResponseContentType string                      `json:"response_content_type"`
	Diagnostic          *cursorBidiAppendDiagnostic `json:"bidi_append,omitempty"`
}

func (p *Proxy) nextRawCapturePaths(captureDir string, host string, path string) (string, string, error) {
	dir := filepath.Join(expandHome(captureDir), "raw", safePathPart(host))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.cursor.raw_capture.mkdir_failed", "dir", dir, "err", err)
		return "", "", fmt.Errorf("create raw capture dir: %w", err)
	}
	seq := p.rawCaptureSeq.Add(1)
	stamp := currentTime().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%s-%06d-%s", stamp, seq, safePathPart(path))
	return filepath.Join(dir, name+".request.raw"), filepath.Join(dir, name+".response.raw"), nil
}

func writeRawCaptureFile(path string, write func(io.Writer) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		slog.Warn("mitm.cursor.raw_capture.open_failed", "path", path, "err", err)
		return 0, fmt.Errorf("open raw capture file: %w", err)
	}
	defer func() { _ = f.Close() }()
	counter := &countingWriter{writer: f, count: 0}
	if err := write(counter); err != nil {
		return counter.count, err
	}
	return counter.count, nil
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.count += int64(n)
	if err != nil {
		return n, fmt.Errorf("write counted bytes: %w", err)
	}
	return n, nil
}

func safePathPart(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "/" {
		return "root"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "capture"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

func appendCursorCaptureMetadata(dir string, meta cursorCaptureMetadata) error {
	now := currentTime()
	event := cursorCaptureEvent{
		Kind:                "cursor_tls_http",
		T:                   now.Unix(),
		TS:                  now.UTC().Format(time.RFC3339Nano),
		Provider:            meta.Provider,
		Host:                meta.Host,
		Method:              meta.Method,
		Path:                meta.Path,
		Status:              meta.Status,
		RequestBytes:        meta.RequestBytes,
		ResponseBytes:       meta.ResponseBytes,
		RequestRawPath:      meta.RequestRawPath,
		ResponseRawPath:     meta.ResponseRawPath,
		RequestID:           meta.RequestID,
		OriginalRequestID:   meta.OriginalRequestID,
		SessionID:           meta.SessionID,
		Traceparent:         meta.Traceparent,
		RequestContentType:  meta.RequestContentType,
		ResponseContentType: meta.ResponseContentType,
		Diagnostic:          meta.Diagnostic,
	}
	return appendCursorCaptureEvent(dir, event)
}

func appendCursorCaptureEvent(dir string, event cursorCaptureEvent) error {
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.cursor.capture.mkdir_failed", "capture_dir", dir, "err", err)
		return fmt.Errorf("create cursor capture dir: %w", err)
	}
	path := filepath.Join(dir, "capture.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("mitm.cursor.capture.open_failed", "path", path, "err", err)
		return fmt.Errorf("open cursor capture metadata: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := json.Marshal(event)
	if err != nil {
		slog.Warn("mitm.cursor.capture.encode_failed", "capture_dir", dir, "err", err)
		return fmt.Errorf("encode cursor capture metadata: %w", err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		slog.Warn("mitm.cursor.capture.write_failed", "path", path, "err", err)
		return fmt.Errorf("write cursor capture metadata: %w", err)
	}
	return nil
}

func extractCursorCaptureHeaders(h http.Header) (requestID string, originalRequestID string, sessionID string, traceparent string) {
	requestID = firstHeader(h, "x-request-id", "x-cursor-request-id", "request-id")
	originalRequestID = firstHeader(h, "x-original-request-id", "x-cursor-original-request-id", "original-request-id")
	sessionID = firstHeader(h, "x-session-id", "x-cursor-session-id", "cursor-session-id")
	traceparent = firstHeader(h, "traceparent")
	return requestID, originalRequestID, sessionID, traceparent
}

func firstHeader(h http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(h.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func headerBlock(statusLine string, h http.Header) []byte {
	var buf bytes.Buffer
	buf.WriteString(statusLine)
	_ = h.Write(&buf)
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
