// Package reorientinject splits a compaction request by token budget. The
// model summarizes the older part, and the split inserts the newest part into
// the summary the model returns. Claude Code stores the whole summary text in
// its isCompactSummary user message and sends that message on later turns.
package reorientinject

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

const (
	messagesPathSuffix     = "/v1/messages"
	eventStreamContentType = "text/event-stream"
)

// Hook runs the splitter as a [mitm.RequestResponseHook]: it rewrites a
// compaction request and its summary response on the MITM proxy.
type Hook struct {
	splitter *Splitter
}

// New constructs a compaction split hook. A nil provider or a nil counter
// yields a hook that leaves every request alone.
func New(provider Provider, settings Settings) *Hook {
	return &Hook{splitter: NewSplitter(provider, settings)}
}

// MatchRequestResponse decides whether this request is a compaction, plans the
// cut, and pairs the request rewrite with the response injection. Every failure
// path forwards the request unmodified and leaves the response unchanged.
func (h *Hook) MatchRequestResponse(
	req mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.Path, messagesPathSuffix) {
		return unmatched(), nil
	}
	// Claude Code declares its own compaction turn on the request. A pasted
	// transcript reproduces the compaction prompt but sets no declaration, so
	// this check precedes the body read and the decode.
	if req.Purpose != mitm.RequestPurposeCompaction {
		return unmatched(), nil
	}
	body, ok := readRequestBody(req.Body)
	if !ok {
		return unmatched(), nil
	}
	result, ok := h.splitter.Plan(body)
	if !ok {
		return unmatched(), nil
	}
	return mitm.RequestResponseHookMatch{
		Matched:            true,
		Transformer:        summaryInjector{splitter: h.splitter, injection: result.Injection},
		RequestTransformer: requestTruncator{forwarded: result.Forwarded},
		ContinueMatching:   false,
	}, nil
}

// readRequestBody returns the request body, or ok=false when the proxy cannot
// produce it. The hook reports no match in that case. Returning the read error
// instead would surface through the seam and fail the request.
func readRequestBody(source mitm.RequestResponseHookBody) ([]byte, bool) {
	body, err := source.Bytes()
	if err != nil {
		slog.Warn("mitm.reorient_inject.request_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		logFallback(fallbackBodyUnreadable, 0)
		return nil, false
	}
	return body, true
}

// requestTruncator hands the proxy the truncated request the splitter built.
type requestTruncator struct {
	forwarded []byte
}

func (t requestTruncator) TransformRequest(context.Context, []byte) ([]byte, bool, error) {
	return t.forwarded, true, nil
}

// summaryInjector inserts the injection into the summary response.
type summaryInjector struct {
	splitter  *Splitter
	injection string
}

func (t summaryInjector) TransformResponse(
	ctx context.Context,
	resp mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	if !streamingSuccess(resp) {
		// The client receives an upstream error body and a non-stream body
		// unchanged.
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.WarnContext(ctx, "mitm.reorient_inject.response_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	injected, err := t.splitter.Inject(body, t.injection)
	if err != nil {
		slog.WarnContext(ctx, "mitm.reorient_inject.summary_unchanged",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	slog.InfoContext(ctx, "mitm.reorient_inject.matched",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
	)
	return responseWithBody(resp, injected), nil
}

// streamingSuccess reports whether the response is a 200 SSE stream, the only
// shape the summary injection is valid for.
func streamingSuccess(resp mitm.ResponseHookResponse) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return strings.Contains(
		strings.ToLower(resp.Header.Get("Content-Type")),
		eventStreamContentType,
	)
}

func responseWithBody(resp mitm.ResponseHookResponse, body []byte) mitm.ResponseHookResponse {
	header := resp.Header.Clone()
	header.Del("Content-Length")
	return mitm.ResponseHookResponse{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Proto:         resp.Proto,
		Header:        header,
		Body:          bytes.NewReader(body),
		ContentLength: -1,
	}
}

func unmatched() mitm.RequestResponseHookMatch {
	return mitm.RequestResponseHookMatch{
		Matched:            false,
		Transformer:        nil,
		RequestTransformer: nil,
		ContinueMatching:   false,
	}
}
