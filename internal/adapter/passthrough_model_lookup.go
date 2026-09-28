package adapter

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

const passthroughModelLookupLimit = 1 << 20

// For a fallback-only model, forwardPassthroughModel sends GET
// <base>/models/<id> to the OpenAI-compatible upstream. It writes a 2xx
// body unchanged, returns model_not_found for an upstream 404, and returns
// an upstream failure for every other status.
func (s *Server) forwardPassthroughModel(ctx context.Context, w http.ResponseWriter, alias adaptermodel.ResolvedAlias, requestedModel string) error {
	var req adapterresolver.ResolvedRequest
	req.PassthroughOverrideName = alias.PassthroughOverride
	req.PassthroughOverride = alias.PassthroughConfig
	req.OpenAICompatPassthrough = alias.OpenAICompatPassthrough
	baseURL, apiKey, modelOverride, _, targetErr := passthroughUpstreamTarget(&req)
	if targetErr != nil {
		return targetErr
	}
	lookupModel := requestedModel
	if modelOverride != "" {
		lookupModel = modelOverride
	}
	target, err := passthroughOverrideTarget(ctx, baseURL, "/models/"+url.PathEscape(lookupModel))
	if err != nil {
		return adapterErrUpstreamFailed("passthrough_override", "resolve fallback model lookup target", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.models.fallback_lookup_request_failed", "concern", "adapter.models.catalog", "err", err)
		return adapterErrUpstreamFailed("passthrough_override", "create fallback model lookup request", err)
	}
	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	response, err := passthroughOverrideDoRequest(request)
	if err != nil {
		return adapterErrUpstreamFailed("passthrough_override", "fallback model lookup failed", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, passthroughModelLookupLimit))
	if err != nil {
		s.log.WarnContext(ctx, "adapter.models.fallback_lookup_read_failed", "concern", "adapter.models.catalog", "err", err)
		return adapterErrUpstreamFailed("passthrough_override", "read fallback model lookup response", err)
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return adapterErrModelNotFound("The model '" + requestedModel + "' does not exist")
	case response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices:
		return adapterErrUpstreamFailed("passthrough_override", "fallback model lookup returned HTTP "+strconv.Itoa(response.StatusCode), nil)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	if _, err := w.Write(body); err != nil {
		s.log.WarnContext(ctx, "adapter.models.fallback_lookup_write_failed", "concern", "adapter.models.catalog", "err", err)
	}
	return nil
}
