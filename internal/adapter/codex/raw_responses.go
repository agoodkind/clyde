package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
)

// RawResponsesRequest preserves a native Codex Responses request at the
// provider boundary. Its body and headers are forwarded without typed
// projection when the ingress has already validated its turn metadata.
type RawResponsesRequest struct {
	Body   []byte
	Header http.Header
	Stream bool
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
	response, err := p.openRawResponsesOnce(ctx, raw, token)
	if err != nil || !shouldRefreshRawResponsesAuth(response.StatusCode, p.auth) {
		return response, err
	}
	refresher, ok := p.auth.(adapterprovider.AuthRefresher)
	if !ok {
		return response, nil
	}
	refreshedToken, refreshErr := refresher.ForceRefresh(ctx)
	if refreshErr != nil {
		return response, nil
	}
	if strings.TrimSpace(refreshedToken) == "" {
		return response, nil
	}
	_ = response.Body.Close()
	return p.openRawResponsesOnce(ctx, raw, refreshedToken)
}

func shouldRefreshRawResponsesAuth(status int, auth adapterprovider.AuthLookup) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	_, ok := auth.(adapterprovider.AuthRefresher)
	return ok
}

func (p *Provider) openRawResponsesOnce(ctx context.Context, raw RawResponsesRequest, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL(p.cfg.BaseURL), bytes.NewReader(raw.Body))
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.build_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("codex provider: build raw Responses request: %w", err)
	}
	req.Header = rawResponsesHeaders(raw.Header, token, p.accountID)
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.raw_responses.request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("codex provider: raw Responses request: %w", err)
	}
	return response, nil
}

func rawResponsesHeaders(inbound http.Header, token, accountID string) http.Header {
	headers := inbound.Clone()
	connectionHeaders := rawResponsesConnectionHeaders(headers)
	for header := range connectionHeaders {
		headers.Del(header)
	}
	for _, header := range []string{
		"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Content-Length", "Proxy-Connection",
	} {
		headers.Del(header)
	}
	headers.Set("Authorization", "Bearer "+token)
	if strings.TrimSpace(accountID) != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
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
