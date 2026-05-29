package mitm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
)

const rawCaptureFileMode os.FileMode = 0o600

type captureConcernInput struct {
	Provider            string
	Host                string
	Method              string
	Path                string
	RequestContentType  string
	ResponseContentType string
}

func resolveCaptureConcern(rules []config.MITMCaptureRouteRule, input captureConcernInput) string {
	if len(rules) == 0 {
		rules = config.DefaultMITMCaptureRouteRules()
	}
	for _, rule := range rules {
		if captureRuleMatches(rule, input) {
			return rule.Concern
		}
	}
	return "unknown"
}

func captureRuleMatches(rule config.MITMCaptureRouteRule, input captureConcernInput) bool {
	if rule.Provider != "" && !strings.EqualFold(string(rule.Provider), strings.TrimSpace(input.Provider)) {
		return false
	}
	if rule.Host != "" && normalizeCaptureHost(rule.Host) != normalizeCaptureHost(input.Host) {
		return false
	}
	if rule.Method != "" && !strings.EqualFold(string(rule.Method), strings.TrimSpace(input.Method)) {
		return false
	}
	if rule.PathExact != "" && rule.PathExact != input.Path {
		return false
	}
	if rule.PathPrefix != "" && !strings.HasPrefix(input.Path, rule.PathPrefix) {
		return false
	}
	if rule.PathContains != "" && !strings.Contains(input.Path, rule.PathContains) {
		return false
	}
	if rule.ContentTypeContains != "" && !captureContentTypeMatches(rule.ContentTypeContains, input) {
		return false
	}
	return rule.Concern != ""
}

func normalizeCaptureHost(host string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
}

func captureContentTypeMatches(needle string, input captureConcernInput) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	requestContentType := strings.ToLower(input.RequestContentType)
	responseContentType := strings.ToLower(input.ResponseContentType)
	return strings.Contains(requestContentType, needle) || strings.Contains(responseContentType, needle)
}

func (p *Proxy) nextRawCapturePaths(captureDir string, concern string, host string, path string) (string, string, error) {
	dir := filepath.Join(expandHome(captureDir), "concerns", safePathPart(concern), "raw", safePathPart(host))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.raw_capture.mkdir_failed", "concern", "providers.mitm.wire", "dir", dir, "err", err)
		return "", "", fmt.Errorf("create raw capture dir: %w", err)
	}
	seq := p.rawCaptureSeq.Add(1)
	stamp := clock.Now().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%s-%06d-%s", stamp, seq, safePathPart(path))
	return filepath.Join(dir, name+".request.raw"), filepath.Join(dir, name+".response.raw"), nil
}

func (p *Proxy) nextHTTPCapturePaths(captureDir string, provider string, path string) (string, string, error) {
	dir := filepath.Join(expandHome(captureDir), "raw", safePathPart(provider))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.raw_capture.mkdir_failed", "concern", "providers.mitm.wire", "dir", dir, "err", err)
		return "", "", fmt.Errorf("create raw capture dir: %w", err)
	}
	seq := p.rawCaptureSeq.Add(1)
	stamp := clock.Now().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%s-%06d-%s", stamp, seq, safePathPart(path))
	return filepath.Join(dir, name+".request.raw"), filepath.Join(dir, name+".response.raw"), nil
}

func writeRawCaptureFile(path string, write func(io.Writer) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		slog.Warn("mitm.raw_capture.open_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
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
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.Trim(out, "-.")
	if out == "" {
		return "capture"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

func (p *Proxy) appendProviderCaptureExtension(dir string, extension CaptureExtension, policy CaptureFilePolicy) error {
	if extension == nil {
		return nil
	}
	dir = expandHome(dir)
	concern := strings.TrimSpace(extension.Concern())
	if concern == "" {
		concern = "unknown"
	}
	raw, err := extension.MarshalJSONLine()
	if err != nil {
		slog.Warn("mitm.provider.capture.encode_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_dir", dir,
			"err", err,
		)
		return fmt.Errorf("encode provider capture metadata: %w", err)
	}
	if err := p.appendProviderCaptureLineAtDir(dir, raw, policy); err != nil {
		return err
	}
	return p.appendProviderCaptureLineAtDir(filepath.Join(dir, "concerns", safePathPart(concern)), raw, policy)
}

func (p *Proxy) appendProviderCaptureLineAtDir(dir string, raw []byte, policy CaptureFilePolicy) error {
	if err := p.writeCaptureLine(dir, raw, policy); err != nil {
		slog.Warn("mitm.provider.capture.write_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_dir", dir,
			"err", err,
		)
		return fmt.Errorf("write provider capture metadata: %w", err)
	}
	return nil
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
