package mitm

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// SetRotatorSink wires the OAuth rotator into the proxy after
// construction. The daemon builds the rotator after MITM startup and
// calls this once with the live rotator instance so the proxy can swap
// bearer tokens and feed Observe signals back. Passing nil restores
// passthrough mode (no bearer swap, no observation).
func (p *Proxy) SetRotatorSink(sink RotatorSink) {
	p.rotatorMu.Lock()
	defer p.rotatorMu.Unlock()
	p.rotatorSink = sink
}

// rotator returns the currently wired RotatorSink under the rotatorMu
// read lock. A nil return means passthrough mode (no swap, no
// observation). Callers MUST treat the returned value as read-only.
func (p *Proxy) rotator() RotatorSink {
	p.rotatorMu.RLock()
	defer p.rotatorMu.RUnlock()
	return p.rotatorSink
}

// maybeRetryOnAuthFailure inspects resp; on HTTP 401 it asks the
// rotator for a recovery token, retries the upstream call once, and
// returns the retry response with the new bearer-derived token.
//
// When no retry is appropriate (non-401 response, no prior swap, nil
// sink, or the rotator declined a fresh token), it returns the
// original resp and swappedToken unchanged so the caller's observe
// path runs against the response actually delivered to the client.
//
// The retry budget is one extra round-trip per inbound request, by
// design: the goal is to recover from a token that went stale between
// selection and dispatch, not to mask repeated upstream failures.
func (p *Proxy) maybeRetryOnAuthFailure(ctx context.Context, resp *http.Response, req upstreamRequest, swappedToken string, sink RotatorSink) (*http.Response, string) {
	if resp.StatusCode != http.StatusUnauthorized || swappedToken == "" || sink == nil {
		return resp, swappedToken
	}
	freshToken, err := sink.TokenAfterAuthFailure(ctx, AnthropicProviderName, swappedToken)
	if err != nil {
		p.log.DebugContext(ctx, "mitm.rotator_swap.retry_skipped",
			"provider", string(AnthropicProviderName),
			"path", req.path,
			"err", err,
		)
		return resp, swappedToken
	}
	if freshToken == "" || freshToken == swappedToken {
		return resp, swappedToken
	}
	retryReq, retryErr := http.NewRequestWithContext(ctx, req.method, req.upstream+req.path, bytes.NewReader(req.body))
	if retryErr != nil {
		p.log.WarnContext(ctx, "mitm.rotator_swap.retry_build_failed",
			"provider", string(AnthropicProviderName),
			"err", retryErr,
		)
		return resp, swappedToken
	}
	copyHeaders(retryReq.Header, req.header)
	retryReq.Header.Set("Authorization", authSchemePrefix+freshToken)
	retryReq.Host = ""
	retryResp, retryDoErr := p.client.Do(retryReq)
	if retryDoErr != nil {
		p.log.WarnContext(ctx, "mitm.rotator_swap.retry_failed",
			"provider", string(AnthropicProviderName),
			"path", req.path,
			"err", retryDoErr,
		)
		return resp, swappedToken
	}
	p.log.InfoContext(ctx, "mitm.rotator_swap.retry_succeeded",
		"provider", string(AnthropicProviderName),
		"path", req.path,
		"retry_status", retryResp.StatusCode,
	)
	_ = resp.Body.Close()
	return retryResp, strings.TrimPrefix(retryReq.Header.Get("Authorization"), authSchemePrefix)
}
