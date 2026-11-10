package adapter

import (
	"context"
	"net/http"
	"time"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

type passthroughRetryFailure struct {
	status       int
	responseBody []byte
	contentType  string
	err          error
}

func newPassthroughRetryFailure(status int, responseBody []byte, contentType string, err error) *passthroughRetryFailure {
	return &passthroughRetryFailure{status: status, responseBody: responseBody, contentType: contentType, err: err}
}

func (s *Server) respondPassthroughRetryFailure(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	streamRequested bool,
	failure *passthroughRetryFailure,
) {
	if failure.err != nil {
		s.respondPassthroughOverrideTransportError(w, r, ctx, req, reqID, started, streamRequested, failure.err)
		return
	}
	s.respondPassthroughOverrideError(
		w,
		r,
		ctx,
		req,
		reqID,
		failure.status,
		failure.responseBody,
		streamRequested,
		failure.contentType,
		started,
	)
}
