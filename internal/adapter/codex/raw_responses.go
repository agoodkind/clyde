package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

// RawResponsesRequest preserves a native Codex Responses request at the
// provider boundary. Its body and headers are forwarded without typed
// projection when the ingress has already validated its turn metadata.
type RawResponsesRequest struct {
	Body        []byte
	Header      http.Header
	RequestID   string
	Correlation correlation.Context
	Stream      bool
}

type rawResponsesAccountLookup interface {
	AccountID(ctx context.Context) (string, error)
}

// HasValidTurnMetadata reports whether the request carries the native Codex
// turn metadata shape required to select the raw forwarding path.
func (r RawResponsesRequest) HasValidTurnMetadata() bool {
	raw := strings.TrimSpace(r.Header.Get(CodexTurnMetadataHeader))
	if raw == "" {
		return false
	}
	var metadata TurnMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return false
	}
	return strings.TrimSpace(metadata.SessionID) != "" &&
		strings.TrimSpace(metadata.ThreadSource) != "" &&
		strings.TrimSpace(metadata.Sandbox) != ""
}

// OpenRawResponses forwards a native Codex Responses request over HTTP without
// rebuilding its JSON payload. A configured Codex credential replaces inbound
// credentials, and one forced refresh retries a rejected credential.
func (p *Provider) OpenRawResponses(ctx context.Context, raw RawResponsesRequest) (*http.Response, error) {
	if p == nil {
		return nil, ErrCodexProviderNotConfigured
	}
	if p.auth == nil {
		return nil, adapterprovider.ErrAuthMissing
	}
	token, err := p.auth.Token(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.auth_lookup_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("codex provider: auth lookup: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, adapterprovider.ErrAuthMissing
	}
	accountID, accountErr := p.rawResponsesAccountID(ctx)
	if accountErr != nil {
		return nil, accountErr
	}
	response, err := p.openRawResponsesAttempt(ctx, raw, token, accountID, 1)
	refresher, shouldRefresh := rawResponsesRefresher(response, err, p.auth)
	if !shouldRefresh {
		return response, err
	}
	refreshedToken, refreshErr := refresher.ForceRefresh(ctx)
	if refreshErr != nil {
		return response, nil
	}
	if strings.TrimSpace(refreshedToken) == "" {
		return response, nil
	}
	_ = response.Body.Close()
	return p.openRawResponsesAttempt(ctx, raw, refreshedToken, accountID, 2)
}

func (p *Provider) rawResponsesAccountID(ctx context.Context) (string, error) {
	lookup, ok := p.auth.(rawResponsesAccountLookup)
	if !ok {
		return "", errors.New("codex provider: auth lookup does not provide account identity")
	}
	accountID, err := lookup.AccountID(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.account_lookup_failed", "concern", "adapter.providers.codex.request", "err", err)
		return "", fmt.Errorf("codex provider: account lookup: %w", err)
	}
	if strings.TrimSpace(accountID) == "" {
		return "", errors.New("codex provider: auth account identity is missing")
	}
	return accountID, nil
}

func rawResponsesRefresher(response *http.Response, requestErr error, auth adapterprovider.AuthLookup) (adapterprovider.AuthRefresher, bool) {
	if requestErr != nil || response == nil || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
		return nil, false
	}
	refresher, ok := auth.(adapterprovider.AuthRefresher)
	return refresher, ok
}

func (p *Provider) openRawResponsesAttempt(ctx context.Context, raw RawResponsesRequest, token, accountID string, attemptNumber int) (*http.Response, error) {
	attemptCtx := ctx
	releaseAttempt := func(string) {}
	if hook := beforeAttemptFromContext(ctx); hook != nil {
		attemptCtx, releaseAttempt = hook(ctx, attemptNumber)
	}
	started := clock.Now()
	response, err := p.openRawResponsesOnce(attemptCtx, raw, token, accountID)
	if err != nil {
		releaseAttempt("codex.raw_responses.attempt.failed")
		return nil, err
	}
	var attemptBody rawResponsesAttemptBody
	attemptBody.ReadCloser = response.Body
	attemptBody.release = releaseAttempt
	attemptBody.log = p.log
	if p.captureStore != nil {
		attemptBody.captureBody = capture.NewCappedBuffer(codexCaptureBodyCap)
		attemptBody.recordCapture = func(responseBody []byte) {
			recordCodexHTTPEgress(
				p.captureStore, raw.Correlation, response.Request, response,
				raw.Body, responseBody, "", started,
			)
		}
	}
	response.Body = &attemptBody
	return response, nil
}

type rawResponsesAttemptBody struct {
	io.ReadCloser
	release       func(string)
	log           *slog.Logger
	once          sync.Once
	captureBody   *capture.CappedBuffer
	recordCapture func([]byte)
	captureOnce   sync.Once
}

func (b *rawResponsesAttemptBody) Read(p []byte) (int, error) {
	count, err := b.ReadCloser.Read(p)
	if count > 0 && b.captureBody != nil {
		_, _ = b.captureBody.Write(p[:count])
	}
	if errors.Is(err, io.EOF) {
		b.finishCapture()
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read raw Responses capture body: %w", err)
	}
	return count, nil
}

func (b *rawResponsesAttemptBody) finishCapture() {
	if b.captureBody == nil || b.recordCapture == nil {
		return
	}
	b.captureOnce.Do(func() { b.recordCapture(b.captureBody.Bytes()) })
}

func (p *Provider) openRawResponsesOnce(ctx context.Context, raw RawResponsesRequest, token, accountID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL(p.cfg.BaseURL), bytes.NewReader(raw.Body))
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.build_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("codex provider: build raw Responses request: %w", err)
	}
	req.Header = rawResponsesHeaders(raw, token, accountID)
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	requestClient := http.Client{
		Transport: client.Transport,
		Jar:       client.Jar,
		Timeout:   client.Timeout,
	}
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := requestClient.Do(req)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("codex provider: raw Responses request: %w", err)
	}
	return response, nil
}

// MarshalWithModel returns a raw request that carries model while retaining every
// other top-level request field. It returns the original bytes when model
// already matches, preserving the native request exactly on the common path.
func (r RawResponsesRequest) MarshalWithModel(model string) (RawResponsesRequest, error) {
	if strings.TrimSpace(model) == "" {
		return RawResponsesRequest{}, errors.New("raw Responses model is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &fields); err != nil {
		return RawResponsesRequest{}, rawResponsesModelRewriteError("decode request", err)
	}
	if fields == nil {
		return RawResponsesRequest{}, rawResponsesModelRewriteError("decode request", errors.New("request must be a JSON object"))
	}
	var current string
	if rawModel, ok := fields["model"]; ok {
		if err := json.Unmarshal(rawModel, &current); err != nil {
			return RawResponsesRequest{}, rawResponsesModelRewriteError("decode model", err)
		}
	}
	if current == model {
		return r, nil
	}
	replacement, err := json.Marshal(model)
	if err != nil {
		return RawResponsesRequest{}, rawResponsesModelRewriteError("encode model", err)
	}
	fields["model"] = replacement
	body, err := json.Marshal(fields)
	if err != nil {
		return RawResponsesRequest{}, rawResponsesModelRewriteError("encode request", err)
	}
	r.Body = body
	return r, nil
}

func rawResponsesModelRewriteError(operation string, err error) error {
	slog.Warn("adapter.codex.raw_responses.model_rewrite_failed", "concern", "adapter.providers.codex.request", "operation", operation, "err", err)
	return fmt.Errorf("raw Responses model rewrite %s: %w", operation, err)
}

func (b *rawResponsesAttemptBody) Close() error {
	err := b.ReadCloser.Close()
	b.finishCapture()
	b.once.Do(func() { b.release("codex.raw_responses.attempt.completed") })
	if err != nil {
		b.log.Warn("adapter.codex.raw_responses.body_close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close raw Codex Responses body: %w", err)
	}
	return nil
}

func rawResponsesHeaders(raw RawResponsesRequest, token, accountID string) http.Header {
	headers, _ := capture.RedactHTTP(raw.Header, nil)
	connectionHeaders := rawResponsesConnectionHeaders(headers)
	for header := range connectionHeaders {
		headers.Del(header)
	}
	for _, header := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Content-Length", "Proxy-Connection",
	} {
		headers.Del(header)
	}
	headers.Set("Authorization", "Bearer "+token)
	if strings.TrimSpace(accountID) != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	corr := raw.Correlation
	if strings.TrimSpace(raw.RequestID) != "" {
		corr = corr.WithRequestID(raw.RequestID)
	}
	for key, values := range clydeingress.HTTPHeaders(corr) {
		headers.Del(key)
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if DetectRawResponsesCompactionProtocol(raw.Header) == RawResponsesCompactionV1 {
		headers.Set("Accept-Encoding", "identity")
	} else if headers.Get("Accept-Encoding") == "" {
		headers.Set("Accept-Encoding", "identity")
	}
	return headers
}

func rawResponsesConnectionHeaders(headers http.Header) map[string]struct{} {
	nominated := make(map[string]struct{})
	for key, values := range headers {
		if !strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			for header := range strings.SplitSeq(value, ",") {
				name := strings.TrimSpace(header)
				if name != "" {
					nominated[name] = struct{}{}
				}
			}
		}
	}
	return nominated
}
