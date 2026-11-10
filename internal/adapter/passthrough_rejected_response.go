package adapter

import (
	"context"
	"io"
	"net/http"
	"time"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

func (s *Server) respondPassthroughRejectedResponse(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	resp *http.Response,
	options passthroughForwardOptions,
	started time.Time,
	contentType string,
) {
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		s.recordPassthroughEgress(ctx, resp, options.body, passthroughCaptureResultFromRead(respBody, readErr), started)
		s.respondPassthroughOverrideTransportError(w, r, ctx, req, options.requestID, started, options.streamRequested, readErr)
		return
	}
	s.recordPassthroughEgress(ctx, resp, options.body, passthroughCaptureResultFromBody(respBody), started)
	s.respondPassthroughOverrideError(w, r, ctx, req, options.requestID, resp.StatusCode, respBody, options.streamRequested, contentType, started)
}
