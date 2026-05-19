package mitm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
)

const (
	hookDefaultTimeout = 5 * time.Minute
	hookTempDirMode    = 0o700
)

// hookEnvelopeRequest is the JSON object clyde writes to the hook
// subprocess's stdin. The hook reads exactly one envelope, performs
// its work, and writes one hookEnvelopeResponse to stdout. The temp
// file paths are absolute and live under the per-request directory
// clyde creates and cleans up. RequestBodyPath is always populated;
// UpstreamResponseStatus, UpstreamResponseHeaders, and
// UpstreamResponseBodyPath are only populated for
// MITMHookModeTransformResponse.
type hookEnvelopeRequest struct {
	Mode                     config.MITMHookMode `json:"mode"`
	RequestID                string              `json:"request_id"`
	RuleName                 string              `json:"rule_name"`
	RequestURL               string              `json:"request_url"`
	RequestMethod            string              `json:"request_method"`
	RequestHeaders           http.Header         `json:"request_headers"`
	RequestBodyPath          string              `json:"request_body_path"`
	UpstreamResponseStatus   int                 `json:"upstream_response_status,omitempty"`
	UpstreamResponseHeaders  http.Header         `json:"upstream_response_headers,omitempty"`
	UpstreamResponseBodyPath string              `json:"upstream_response_body_path,omitempty"`
	OutputBodyPath           string              `json:"output_body_path"`
}

// hookEnvelopeResponse is the JSON object the hook subprocess writes
// to stdout after it finishes producing its output body. clyde reads
// the body bytes from the OutputBodyPath named in the request
// envelope; the response envelope only carries the HTTP status and
// any header overrides the hook wants on the wire.
type hookEnvelopeResponse struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers,omitempty"`
}

// matchHookRule walks the rule table in order and returns the first
// rule whose host, method, and path predicates all match the
// intercepted request. The second return is false when no rule
// matches. The function never panics on a malformed regex; a
// previously-validated rule may be rejected at config load to keep
// this path branch-free.
func matchHookRule(rules []config.MITMHookRule, host, method, path string) (config.MITMHookRule, bool) {
	for _, rule := range rules {
		if rule.Command == "" {
			continue
		}
		if !hookHostMatches(rule.MatchHost, host) {
			continue
		}
		if rule.MatchMethod != "" && !strings.EqualFold(strings.TrimSpace(rule.MatchMethod), method) {
			continue
		}
		if rule.MatchPathRegex != "" {
			re, err := regexp.Compile(rule.MatchPathRegex)
			if err != nil {
				continue
			}
			if !re.MatchString(path) {
				continue
			}
		}
		return rule, true
	}
	return config.MITMHookRule{
		Name:           "",
		MatchHost:      "",
		MatchPathRegex: "",
		MatchMethod:    "",
		Mode:           "",
		Command:        "",
		Args:           nil,
		Timeout:        0,
	}, false
}

// hookHostMatches implements the literal-or-suffix rule clyde uses
// for the MatchHost field. An empty rule matches every host. A rule
// of the form "*.example.com" matches every subdomain. Any other
// literal must equal the host exactly (case-insensitively).
func hookHostMatches(pattern, host string) bool {
	if pattern == "" {
		return true
	}
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return strings.HasSuffix(host, suffix)
	}
	return pattern == host
}

// hookStaging is the per-request directory clyde creates to hold the
// temp files exchanged with the hook subprocess. The directory and
// every file inside it are removed when cleanup runs, regardless of
// whether the hook succeeded.
type hookStaging struct {
	dir              string
	requestBodyPath  string
	upstreamBodyPath string
	outputBodyPath   string
}

func newHookStaging(log *slog.Logger, baseDir, requestID string) (*hookStaging, error) {
	dir := filepath.Join(baseDir, requestID)
	if err := os.MkdirAll(dir, hookTempDirMode); err != nil {
		log.Warn("mitm.hook.staging_create_failed", "dir", dir, "err", err)
		return nil, fmt.Errorf("create hook staging dir %s: %w", dir, err)
	}
	return &hookStaging{
		dir:              dir,
		requestBodyPath:  filepath.Join(dir, "request.body"),
		upstreamBodyPath: filepath.Join(dir, "upstream.body"),
		outputBodyPath:   filepath.Join(dir, "output.body"),
	}, nil
}

func (h *hookStaging) cleanup(log *slog.Logger) {
	if err := os.RemoveAll(h.dir); err != nil {
		log.Warn("mitm.hook.staging_cleanup_failed", "dir", h.dir, "err", err)
	}
}

// hookDispatch carries the resolved per-request context the
// dispatcher passes into runHook. The capture sink is invoked once
// with the rewritten response bytes so the existing capture pipeline
// still records every hook-mediated exchange.
type hookDispatch struct {
	rule       config.MITMHookRule
	requestID  string
	request    *http.Request
	requestRaw []byte
	upstream   *hookUpstream
	staging    *hookStaging
	log        *slog.Logger
}

// hookUpstream is the upstream response payload clyde captured before
// invoking the hook in transform_response mode. The Body slice owns a
// fully buffered copy because the hook envelope writes the bytes to
// disk. For synthesize and transform_request mode the value is nil.
type hookUpstream struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// runHook stages the temp files, writes the bodies, forks the
// configured binary, waits for the response envelope, and returns the
// HTTP response that the caller streams back to the client.
func runHook(ctx context.Context, d *hookDispatch) (*hookResult, error) {
	timeout := d.rule.Timeout.AsDuration()
	if timeout <= 0 {
		timeout = hookDefaultTimeout
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	envelope, err := stageHookEnvelope(d)
	if err != nil {
		return nil, err
	}
	stdinBytes, err := json.Marshal(envelope)
	if err != nil {
		d.log.WarnContext(ctx, "mitm.hook.envelope_marshal_failed", "rule", d.rule.Name, "err", err)
		return nil, fmt.Errorf("marshal hook envelope: %w", err)
	}
	started := currentTime()
	d.log.InfoContext(ctx, "mitm.hook.spawn",
		"rule", d.rule.Name,
		"command", d.rule.Command,
		"mode", envelope.Mode,
		"request_id", d.requestID,
	)
	stdout, runErr := execHookCommand(hookCtx, d.rule.Command, d.rule.Args, stdinBytes)
	if runErr != nil {
		d.log.WarnContext(ctx, "mitm.hook.spawn_failed",
			"rule", d.rule.Name,
			"command", d.rule.Command,
			"err", runErr,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		return nil, fmt.Errorf("hook %q: %w", d.rule.Name, runErr)
	}
	resp := hookEnvelopeResponse{Status: 0, Headers: nil}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		d.log.WarnContext(ctx, "mitm.hook.envelope_invalid",
			"rule", d.rule.Name,
			"stdout", stdout,
			"err", err,
		)
		return nil, fmt.Errorf("parse hook response envelope: %w", err)
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	bodyInfo, err := os.Stat(d.staging.outputBodyPath)
	if err != nil {
		d.log.WarnContext(ctx, "mitm.hook.output_stat_failed", "rule", d.rule.Name, "path", d.staging.outputBodyPath, "err", err)
		return nil, fmt.Errorf("stat hook output body: %w", err)
	}
	bodyFile, err := os.Open(d.staging.outputBodyPath)
	if err != nil {
		d.log.WarnContext(ctx, "mitm.hook.output_open_failed", "rule", d.rule.Name, "path", d.staging.outputBodyPath, "err", err)
		return nil, fmt.Errorf("open hook output body: %w", err)
	}
	d.log.InfoContext(ctx, "mitm.hook.completed",
		"rule", d.rule.Name,
		"status", resp.Status,
		"output_bytes", bodyInfo.Size(),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return &hookResult{
		Status:        resp.Status,
		Headers:       resp.Headers,
		Body:          bodyFile,
		ContentLength: bodyInfo.Size(),
	}, nil
}

// stageHookEnvelope writes the request body (and the upstream body
// for transform_response mode) to the staging directory and returns
// the envelope clyde sends to the hook on stdin. Splitting this out
// keeps runHook below the funlen ceiling.
func stageHookEnvelope(d *hookDispatch) (hookEnvelopeRequest, error) {
	if err := os.WriteFile(d.staging.requestBodyPath, d.requestRaw, 0o600); err != nil {
		d.log.Warn("mitm.hook.request_body_write_failed",
			"rule", d.rule.Name,
			"path", d.staging.requestBodyPath,
			"err", err,
		)
		return hookEnvelopeRequest{
			Mode:                     "",
			RequestID:                "",
			RuleName:                 "",
			RequestURL:               "",
			RequestMethod:            "",
			RequestHeaders:           nil,
			RequestBodyPath:          "",
			UpstreamResponseStatus:   0,
			UpstreamResponseHeaders:  nil,
			UpstreamResponseBodyPath: "",
			OutputBodyPath:           "",
		}, fmt.Errorf("write request body for hook: %w", err)
	}
	envelope := hookEnvelopeRequest{
		Mode:                     resolveHookMode(d.rule.Mode),
		RequestID:                d.requestID,
		RuleName:                 d.rule.Name,
		RequestURL:               d.request.URL.String(),
		RequestMethod:            d.request.Method,
		RequestHeaders:           d.request.Header.Clone(),
		RequestBodyPath:          d.staging.requestBodyPath,
		UpstreamResponseStatus:   0,
		UpstreamResponseHeaders:  nil,
		UpstreamResponseBodyPath: "",
		OutputBodyPath:           d.staging.outputBodyPath,
	}
	if d.upstream != nil {
		if err := os.WriteFile(d.staging.upstreamBodyPath, d.upstream.Body, 0o600); err != nil {
			d.log.Warn("mitm.hook.upstream_body_write_failed",
				"rule", d.rule.Name,
				"path", d.staging.upstreamBodyPath,
				"err", err,
			)
			return hookEnvelopeRequest{
				Mode:                     "",
				RequestID:                "",
				RuleName:                 "",
				RequestURL:               "",
				RequestMethod:            "",
				RequestHeaders:           nil,
				RequestBodyPath:          "",
				UpstreamResponseStatus:   0,
				UpstreamResponseHeaders:  nil,
				UpstreamResponseBodyPath: "",
				OutputBodyPath:           "",
			}, fmt.Errorf("write upstream body for hook: %w", err)
		}
		envelope.UpstreamResponseStatus = d.upstream.Status
		envelope.UpstreamResponseHeaders = d.upstream.Headers.Clone()
		envelope.UpstreamResponseBodyPath = d.staging.upstreamBodyPath
	}
	return envelope, nil
}

// execHookCommand forks the configured hook binary, pipes the JSON
// envelope on stdin, and returns the captured stdout. The command
// path must be absolute so the trust boundary lines up with the
// operator-owned clyde TOML config that supplied the value; the
// absolute-path check matches the surrounding callers' assumption
// that hook paths are resolved up front and not derived from a
// search PATH at run time.
func execHookCommand(ctx context.Context, command string, extraArgs []string, stdinBytes []byte) (string, error) {
	slog.DebugContext(ctx, "mitm.hook.exec",
		"command", command,
		"argv_count", len(extraArgs),
	)
	if !filepath.IsAbs(command) {
		slog.WarnContext(ctx, "mitm.hook.exec_relative_path_rejected", "command", command)
		return "", fmt.Errorf("hook command must be an absolute path: %s", command)
	}
	args := make([]string, 0, len(extraArgs))
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(string(stdinBytes))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "mitm.hook.exec_failed",
			"command", command,
			"err", err,
			"stderr", stderr.String(),
		)
		return stdout.String(), fmt.Errorf("hook subprocess: %w (stderr=%q)", err, stderr.String())
	}
	return stdout.String(), nil
}

// hookResult is the in-memory response the dispatcher hands back to
// the cursor request handler. Body is an open file the caller is
// responsible for closing after streaming it to the client.
type hookResult struct {
	Status        int
	Headers       http.Header
	Body          io.ReadCloser
	ContentLength int64
}

func resolveHookMode(m config.MITMHookMode) config.MITMHookMode {
	if m == "" {
		return config.MITMHookModeTransformResponse
	}
	return m
}

// writeHookResponse streams the hook's output body to the client
// connection, simultaneously appending the bytes to responseRawPath
// so the existing capture pipeline still records the rewritten
// response verbatim. The total bytes written to the client (header +
// body) are returned for the capture metadata's response_bytes
// field.
func writeHookResponse(client *bufio.Writer, result *hookResult, responseRawPath string, log *slog.Logger) (int64, error) {
	responseFile, err := os.OpenFile(responseRawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		log.Warn("mitm.hook.response.open_capture_failed", "path", responseRawPath, "err", err)
		return 0, fmt.Errorf("open raw hook response: %w", err)
	}
	defer func() { _ = responseFile.Close() }()

	headers := http.Header{}
	if result.Headers != nil {
		headers = result.Headers.Clone()
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/octet-stream")
	}
	headers.Del("Transfer-Encoding")
	headers.Set("Content-Length", strconv.FormatInt(result.ContentLength, 10))

	statusLine := "HTTP/1.1 " + strconv.Itoa(result.Status) + " " + http.StatusText(result.Status) + "\r\n"
	header := headerBlock(statusLine, headers)
	written, err := writeCursorResponseBytes(client, responseFile, header, "hook header")
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := result.Body.Read(buf)
		if n > 0 {
			count, err := writeCursorResponseBytes(client, responseFile, buf[:n], "hook body")
			written += count
			if err != nil {
				return written, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			if err := client.Flush(); err != nil {
				log.Warn("mitm.hook.response.flush_failed", "err", err)
				return written, fmt.Errorf("flush hook response: %w", err)
			}
			return written, nil
		}
		if readErr != nil {
			log.Warn("mitm.hook.response.read_failed", "err", readErr)
			return written, fmt.Errorf("read hook output body: %w", readErr)
		}
	}
}

// hookRequestIDCounter mints request ids for the per-hook staging
// directory. The id only needs to be unique within the running clyde
// process; concurrent hooks need distinct dirs to avoid clobbering
// each other's temp files. Monotonic counter plus the start time
// gives both uniqueness and at-a-glance ordering in the logs.
var (
	hookRequestIDMu      sync.Mutex
	hookRequestIDCounter uint64
)

func nextHookRequestID() string {
	hookRequestIDMu.Lock()
	defer hookRequestIDMu.Unlock()
	hookRequestIDCounter++
	return strconv.FormatInt(currentTime().UnixNano(), 36) + "-" + strconv.FormatUint(hookRequestIDCounter, 36)
}

// hookedCursorParams carries the per-request state the standard
// Cursor request handler already computed (concern, raw capture
// paths, request bytes) so the hook variant can reuse them without
// recomputing. Bundling them in a struct keeps the call site readable
// and avoids a ten-argument helper.
type hookedCursorParams struct {
	writer          *bufio.Writer
	req             *http.Request
	body            []byte
	target          string
	host            string
	rule            config.MITMHookRule
	started         time.Time
	concern         string
	requestBytes    int64
	requestRawPath  string
	responseRawPath string
	cfg             config.MITMConfig
	capturePolicy   CaptureFilePolicy
}

// runHookedCursorRequest runs the Cursor request through the matched
// hook rule and writes the resulting response on the client
// connection. The function owns the capture-metadata write for the
// rewritten exchange so the standard handler's capture path stays
// untouched.
func (p *Proxy) runHookedCursorRequest(ctx context.Context, params hookedCursorParams) error {
	stagingRoot := DefaultHookStagingRoot()
	requestID := nextHookRequestID()
	staging, err := newHookStaging(p.log, stagingRoot, requestID)
	if err != nil {
		return err
	}
	defer staging.cleanup(p.log)

	upstream, upstreamErr := p.maybeFetchUpstreamForHook(params)
	if upstreamErr != nil {
		return upstreamErr
	}

	result, err := runHook(ctx, &hookDispatch{
		rule:       params.rule,
		requestID:  requestID,
		request:    params.req,
		requestRaw: params.body,
		upstream:   upstream,
		staging:    staging,
		log:        p.log,
	})
	if err != nil {
		p.writeHookErrorResponse(params.writer, err)
		return err
	}
	defer func() { _ = result.Body.Close() }()

	responseBytes, err := writeHookResponse(params.writer, result, params.responseRawPath, p.log)
	if err != nil {
		return err
	}
	p.recordHookedCaptureMetadata(params, result, responseBytes)
	p.log.InfoContext(ctx, "mitm.cursor.hook.completed",
		"host", params.host,
		"hook", params.rule.Name,
		"path", params.req.URL.Path,
		"status", result.Status,
		"duration_ms", time.Since(params.started).Milliseconds(),
		"request_bytes", params.requestBytes,
		"response_bytes", responseBytes,
	)
	return nil
}

// recordHookedCaptureMetadata writes the capture.jsonl entry for a
// hook-rewritten request. The Hook field is set so downstream tooling
// can distinguish hook-mediated rewrites from plain pass-through
// exchanges.
func (p *Proxy) recordHookedCaptureMetadata(params hookedCursorParams, result *hookResult, responseBytes int64) {
	requestID, originalRequestID, sessionID, traceparent := extractCursorCaptureHeaders(params.req.Header)
	if appendErr := p.appendCursorCaptureMetadata(params.cfg.CaptureDir, cursorCaptureMetadata{
		Provider:            "cursor",
		Concern:             params.concern,
		Host:                params.host,
		Path:                params.req.URL.Path,
		Method:              params.req.Method,
		Status:              result.Status,
		RequestBytes:        params.requestBytes,
		ResponseBytes:       responseBytes,
		RequestRawPath:      params.requestRawPath,
		ResponseRawPath:     params.responseRawPath,
		RequestID:           requestID,
		OriginalRequestID:   originalRequestID,
		SessionID:           sessionID,
		Traceparent:         traceparent,
		RequestContentType:  params.req.Header.Get("Content-Type"),
		ResponseContentType: result.Headers.Get("Content-Type"),
		Diagnostic:          nil,
		Hook:                params.rule.Name,
	}, params.capturePolicy); appendErr != nil {
		p.log.Warn("mitm.cursor.hook.capture.append_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"hook", params.rule.Name,
			"capture_dir", params.cfg.CaptureDir,
			"err", appendErr,
		)
	}
}

// maybeFetchUpstreamForHook performs the upstream round-trip when the
// hook mode needs it. For transform_response the caller wants the
// upstream body to pass to the hook. For synthesize and
// transform_request modes upstream is skipped and the return is nil.
func (p *Proxy) maybeFetchUpstreamForHook(params hookedCursorParams) (*hookUpstream, error) {
	mode := resolveHookMode(params.rule.Mode)
	if mode != config.MITMHookModeTransformResponse {
		return nil, nil
	}
	resp, err := p.cursorUpstreamRoundTrip(params.req, params.body, params.target, params.host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.log.Warn("mitm.cursor.hook.upstream_read_failed",
			"host", params.host,
			"path", params.req.URL.Path,
			"err", err,
		)
		return nil, fmt.Errorf("read upstream body for hook: %w", err)
	}
	return &hookUpstream{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    body,
	}, nil
}

// writeHookErrorResponse writes a minimal 502 to the client when the
// hook fails to fork, times out, or returns an unparseable envelope.
// The body is empty so Squirrel.Mac and any other downstream
// downloader sees a clean error rather than a half-streamed
// truncated body.
func (p *Proxy) writeHookErrorResponse(client *bufio.Writer, hookErr error) {
	status := http.StatusBadGateway
	body := []byte("hook failed: " + hookErr.Error() + "\n")
	headers := http.Header{}
	headers.Set("Content-Type", "text/plain; charset=utf-8")
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	statusLine := "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n"
	if _, err := client.WriteString(statusLine); err != nil {
		p.log.Warn("mitm.cursor.hook.error_write_status_failed", "err", err)
		return
	}
	for key, values := range headers {
		for _, value := range values {
			if _, err := client.WriteString(key + ": " + value + "\r\n"); err != nil {
				p.log.Warn("mitm.cursor.hook.error_write_header_failed", "err", err)
				return
			}
		}
	}
	if _, err := client.WriteString("\r\n"); err != nil {
		p.log.Warn("mitm.cursor.hook.error_write_header_end_failed", "err", err)
		return
	}
	if _, err := client.Write(body); err != nil {
		p.log.Warn("mitm.cursor.hook.error_write_body_failed", "err", err)
		return
	}
	if err := client.Flush(); err != nil {
		p.log.Warn("mitm.cursor.hook.error_flush_failed", "err", err)
	}
}
