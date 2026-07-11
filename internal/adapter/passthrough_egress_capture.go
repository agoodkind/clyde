package adapter

import (
	"context"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

func (s *Server) recordPassthroughEgress(ctx context.Context, resp *http.Response, requestBody []byte, result passthroughCaptureResult, started time.Time) {
	if resp == nil {
		return
	}
	s.recordPassthroughEgressAttempt(ctx, resp.Request, resp, requestBody, result, started)
}

func (s *Server) recordPassthroughEgressAttempt(
	ctx context.Context,
	request *http.Request,
	response *http.Response,
	requestBody []byte,
	result passthroughCaptureResult,
	started time.Time,
) {
	if s.deps.CaptureStore == nil || request == nil || request.URL == nil {
		return
	}
	requestHeaders := sanitizedPassthroughRequestHeaders(request.Header)
	status, responseHeaders, upstreamRequestID, responseType := passthroughCaptureResponseMetadata(response)
	s.deps.CaptureStore.RecordCappedExchange(correlation.FromContext(ctx), capture.Exchange{
		Client: "adapter.passthrough", Provider: "openai-compatible", Concern: "adapter.passthrough.egress",
		Host: request.URL.Host, Method: request.Method, Path: request.URL.Path, Status: status,
		UpstreamRequestID: upstreamRequestID, SessionID: "", RequestHeaders: requestHeaders,
		ResponseHeaders: responseHeaders, RequestBody: requestBody, ResponseBody: result.body,
		RequestType: request.Header.Get("Content-Type"), ResponseType: responseType, Started: started,
	}, result.totalBytes, result.truncated)
}

func sanitizedPassthroughRequestHeaders(headers http.Header) http.Header {
	sanitized := headers.Clone()
	for name := range sanitized {
		if redactedHeader(strings.ToLower(name)) {
			sanitized.Del(name)
		}
	}
	return sanitized
}

func passthroughCaptureResponseMetadata(response *http.Response) (int, http.Header, string, string) {
	if response == nil {
		return 0, make(http.Header), "", ""
	}
	return response.StatusCode, sanitizedCaptureResponseHeaders(response.Header), passthroughUpstreamRequestID(response.Header), response.Header.Get("Content-Type")
}

func passthroughUpstreamRequestID(headers http.Header) string {
	if requestID := headers.Get("X-Request-Id"); requestID != "" {
		return requestID
	}
	return headers.Get("Request-Id")
}
