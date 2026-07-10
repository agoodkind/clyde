package mitm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// RequestResponseHook is an in-process MITM extension point for selecting a
// response-body transform from an intercepted request. The request body is
// exposed lazily so hooks can reject by method, path, headers, provider, or
// host without forcing the proxy to consume and rewind streaming request bodies.
type RequestResponseHook interface {
	MatchRequestResponse(RequestResponseHookRequest) (RequestResponseHookMatch, error)
}

// RequestResponseHookRequest exposes the request shape a hook can match. Body
// reads are cached and the proxy rewinds the request body before forwarding it
// upstream, so matching does not steal bytes from the real upstream request.
type RequestResponseHookRequest struct {
	Provider string
	Host     string
	Method   string
	Path     string
	Header   http.Header
	Body     RequestResponseHookBody
}

// RequestResponseHookBody provides lazy, cached request-body access to hooks.
type RequestResponseHookBody interface {
	Bytes() ([]byte, error)
}

// RequestResponseHookMatch is the typed match result returned by a hook. A
// matched hook must carry at least one transformer. RequestTransformer, when
// set, rewrites the request body before it is forwarded upstream; Transformer,
// when set, rewrites the response stream returned to the client. A hook may set
// both, as the reorient injection hook does (it trims the summarization request
// and appends to the summary response from the same match).
type RequestResponseHookMatch struct {
	Matched            bool
	Transformer        ResponseTransformer
	RequestTransformer RequestTransformer
}

// ResponseTransformer rewrites the upstream response stream returned to the
// client. A transformer can wrap ResponseHookResponse.Body with [io.MultiReader]
// or another streaming reader to append SSE bytes before the stream ends.
type ResponseTransformer interface {
	TransformResponse(context.Context, ResponseHookResponse) (ResponseHookResponse, error)
}

// RequestTransformer rewrites the request body before the proxy forwards it
// upstream. It receives the fully buffered request body and returns the new
// body plus a changed flag; changed=false leaves the request untouched. An
// error is treated as fail-open by the proxy: the original request body is
// forwarded unchanged so a transform bug can never break the client request.
type RequestTransformer interface {
	TransformRequest(ctx context.Context, body []byte) (newBody []byte, changed bool, err error)
}

// ResponseHookResponse is the client-visible response shape a transformer can
// return. The capture pipeline records the post-transform bytes streamed from
// Body, capped by the existing capture-store body limit.
type ResponseHookResponse struct {
	StatusCode    int
	Status        string
	Proto         string
	Header        http.Header
	Body          io.Reader
	ContentLength int64
}

type cachedRequestResponseHookBody struct {
	mu      sync.Mutex
	request *http.Request
	loaded  bool
	body    []byte
	err     error
}

type staticRequestResponseHookBody struct {
	body []byte
}

// SetRequestResponseHooks replaces the in-process MITM response hook registry.
// The slice is copied so later caller mutations cannot alter in-flight proxy
// state. Existing in-flight requests keep the snapshot they already selected.
func (p *Proxy) SetRequestResponseHooks(hooks []RequestResponseHook) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requestResponseHooks = slices.Clone(hooks)
}

func (p *Proxy) requestResponseHookSnapshot() []RequestResponseHook {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.requestResponseHooks)
}

func (p *Proxy) matchRequestResponseHook(request RequestResponseHookRequest) (ResponseTransformer, RequestTransformer, error) {
	for _, hook := range p.requestResponseHookSnapshot() {
		if hook == nil {
			continue
		}
		match, err := hook.MatchRequestResponse(request)
		if err != nil {
			slog.Warn("mitm.response_hook.match_failed", "concern", "providers.mitm.wire", "provider", request.Provider, "host", request.Host, "method", request.Method, "path", request.Path, "err", err)
			return nil, nil, fmt.Errorf("match request response hook: %w", err)
		}
		if !match.Matched {
			continue
		}
		if match.Transformer == nil && match.RequestTransformer == nil {
			return nil, nil, fmt.Errorf("matched request response hook returned no transformer")
		}
		return match.Transformer, match.RequestTransformer, nil
	}
	return nil, nil, nil
}

func newRequestResponseHookRequest(provider string, host string, req *http.Request, body RequestResponseHookBody) RequestResponseHookRequest {
	return RequestResponseHookRequest{
		Provider: provider,
		Host:     host,
		Method:   req.Method,
		// Path is the request path without the query string, so a hook matching
		// on a path suffix is not defeated by a query-parameterized request such
		// as /v1/messages?beta=...
		Path:   req.URL.Path,
		Header: req.Header.Clone(),
		Body:   body,
	}
}

func newCachedRequestResponseHookBody(req *http.Request) *cachedRequestResponseHookBody {
	return &cachedRequestResponseHookBody{
		mu:      sync.Mutex{},
		request: req,
		loaded:  false,
		body:    nil,
		err:     nil,
	}
}

func newStaticRequestResponseHookBody(body []byte) staticRequestResponseHookBody {
	return staticRequestResponseHookBody{body: append([]byte(nil), body...)}
}

func (b *cachedRequestResponseHookBody) Bytes() ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		b.body, b.err = readAndRewindHookRequestBody(b.request)
		b.loaded = true
	}
	if b.err != nil {
		return nil, b.err
	}
	return append([]byte(nil), b.body...), nil
}

func (b staticRequestResponseHookBody) Bytes() ([]byte, error) {
	return append([]byte(nil), b.body...), nil
}

func readAndRewindHookRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	body, readErr := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if readErr != nil {
		// The body could not be fully read, so reseating the partial buffer would
		// forward a truncated request upstream (hook match errors are non-fatal
		// and forwarding continues). Reseat a body that re-surfaces the error so
		// the upstream send fails cleanly instead of sending a partial request.
		req.Body = io.NopCloser(&erroringBodyReader{err: readErr})
		req.ContentLength = -1
		req.GetBody = func() (io.ReadCloser, error) {
			return nil, fmt.Errorf("hook request body read failed: %w", readErr)
		}
		slog.Warn("mitm.response_hook.request_body_read_failed", "concern", "providers.mitm.wire", "err", readErr)
		return body, fmt.Errorf("read hook request body: %w", readErr)
	}
	reseatRequestBody(req, body)
	if closeErr != nil {
		slog.Warn("mitm.response_hook.request_body_close_failed", "concern", "providers.mitm.wire", "err", closeErr)
		return body, fmt.Errorf("close hook request body: %w", closeErr)
	}
	return body, nil
}

// reseatRequestBody seats body as the request body behind a fresh reader,
// recomputes ContentLength, and installs a GetBody that re-reads the same bytes.
// It is used both to rewind the original body after hook matching and to install
// a body that a RequestTransformer rewrote.
func reseatRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

// transformRequestBody applies a RequestTransformer to body and returns the new
// body plus whether it changed. It is fail-open: a nil transformer, a no-op
// transform, or a transform error all return the original body with changed
// false, so a request rewrite can never break the forwarded request.
func (p *Proxy) transformRequestBody(ctx context.Context, transformer RequestTransformer, body []byte) ([]byte, bool) {
	if transformer == nil {
		return body, false
	}
	newBody, changed, err := transformer.TransformRequest(ctx, body)
	if err != nil {
		slog.WarnContext(ctx, "mitm.request_hook.transform_failed", "concern", "providers.mitm.wire", "err", err)
		return body, false
	}
	if !changed {
		return body, false
	}
	return newBody, true
}

// erroringBodyReader is a request body that surfaces a fixed error on every read,
// so a request whose body could not be fully read fails the upstream send rather
// than forwarding a truncated body.
type erroringBodyReader struct {
	err error
}

func (r *erroringBodyReader) Read([]byte) (int, error) {
	return 0, r.err
}

func (p *Proxy) applyResponseHook(ctx context.Context, transformer ResponseTransformer, resp *http.Response) (*http.Response, error) {
	if transformer == nil {
		return resp, nil
	}
	// A response transformer rewrites the body, so it must read a decoded body.
	// Streaming-decode the matched response in place and strip the content
	// encoding so the transformer reads a plain body without the seam buffering
	// the whole response. When the encoding cannot be decoded (for example
	// brotli), skip the transform and forward the original response so a
	// compressed body is never handed back mislabeled.
	if !decodeResponseBodyForHook(resp) {
		// The body was not decoded: an encoding the proxy cannot handle (brotli),
		// or a supported encoding whose bytes are mislabeled or truncated. Forward
		// the original response untouched either way.
		slog.WarnContext(ctx, "mitm.response_hook.decode_declined", "concern", "providers.mitm.wire", "encoding", resp.Header.Get("Content-Encoding"))
		return resp, nil
	}
	output, err := transformer.TransformResponse(ctx, responseHookResponseFromHTTP(resp))
	if err != nil {
		slog.WarnContext(ctx, "mitm.response_hook.transform_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("transform response hook: %w", err)
	}
	if output.Body == nil {
		return nil, fmt.Errorf("transform response hook returned nil body")
	}
	return httpResponseFromHookResponse(resp, output), nil
}

// decodeResponseBodyForHook prepares a matched response body for a transformer
// without buffering the whole response. It leaves an unencoded body untouched
// and wraps a body that carries a Content-Encoding the proxy can decode (gzip,
// deflate, zstd) in a streaming decompressor with the encoding headers stripped,
// so a transformer reads (and re-emits) a plain body. It returns false without
// touching the body when the encoding cannot be decoded (for example brotli), so
// the caller forwards the original response untouched.
func decodeResponseBodyForHook(resp *http.Response) bool {
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return true
	}
	// Buffer the body so the decoder can peek its magic bytes without consuming
	// from the underlying stream. On a declined encoding the same buffered reader
	// becomes the body, so bytes buffered during detection are never lost and the
	// caller forwards the original response intact.
	buffered := bufio.NewReader(resp.Body)
	original := resp.Body
	decoder, ok := newDecompressingReadCloser(buffered, original, encoding)
	if !ok {
		resp.Body = &bufferedReadCloser{reader: buffered, closer: original}
		return false
	}
	resp.Body = decoder
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return true
}

func responseHookResponseFromHTTP(resp *http.Response) ResponseHookResponse {
	return ResponseHookResponse{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Proto:      resp.Proto,
		Header:     resp.Header.Clone(),
		Body:       resp.Body,
		// A transformer is about to rewrite the body, so its length is unknown to
		// the seam. Hand it -1 by default; a transformer that keeps a fixed length
		// can set one on the response it returns.
		ContentLength: -1,
	}
}

func httpResponseFromHookResponse(base *http.Response, response ResponseHookResponse) *http.Response {
	out := new(http.Response)
	*out = *base
	out.StatusCode = hookStatusCode(base, response)
	out.Status = hookStatus(out.StatusCode, response.Status)
	out.Proto = hookProto(base, response)
	out.Header = response.Header.Clone()
	if out.Header == nil {
		out.Header = http.Header{}
	}
	out.Body = io.NopCloser(response.Body)
	out.ContentLength = response.ContentLength
	if out.ContentLength < 0 {
		out.Header.Del("Content-Length")
	}
	return out
}

func hookStatusCode(base *http.Response, response ResponseHookResponse) int {
	if response.StatusCode != 0 {
		return response.StatusCode
	}
	return base.StatusCode
}

func hookStatus(statusCode int, status string) string {
	if status != "" {
		return status
	}
	text := http.StatusText(statusCode)
	if text == "" {
		return strconv.Itoa(statusCode)
	}
	return fmt.Sprintf("%d %s", statusCode, text)
}

func hookProto(base *http.Response, response ResponseHookResponse) string {
	if response.Proto != "" {
		return response.Proto
	}
	return base.Proto
}
