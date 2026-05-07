package mitm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

func (p *Proxy) nextRawCapturePaths(captureDir string, host string, path string) (string, string, error) {
	dir := filepath.Join(expandHome(captureDir), "raw", safePathPart(host))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	seq := p.rawCaptureSeq.Add(1)
	stamp := currentTime().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%s-%06d-%s", stamp, seq, safePathPart(path))
	return filepath.Join(dir, name+".request.raw"), filepath.Join(dir, name+".response.raw"), nil
}

func writeRawCaptureFile(path string, write func(io.Writer) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	counter := &countingWriter{writer: f}
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
	return n, err
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
	event := map[string]any{
		"kind":                  "cursor_tls_http",
		"t":                     currentTime().Unix(),
		"ts":                    currentTime().UTC().Format(time.RFC3339Nano),
		"provider":              meta.Provider,
		"host":                  meta.Host,
		"method":                meta.Method,
		"path":                  meta.Path,
		"status":                meta.Status,
		"request_bytes":         meta.RequestBytes,
		"response_bytes":        meta.ResponseBytes,
		"request_raw_path":      meta.RequestRawPath,
		"response_raw_path":     meta.ResponseRawPath,
		"request_id":            meta.RequestID,
		"original_request_id":   meta.OriginalRequestID,
		"session_id":            meta.SessionID,
		"traceparent":           meta.Traceparent,
		"request_content_type":  meta.RequestContentType,
		"response_content_type": meta.ResponseContentType,
	}
	if meta.Diagnostic != nil {
		event["bidi_append"] = meta.Diagnostic
	}
	return appendCapture(dir, event)
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
