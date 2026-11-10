package mitm

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

func (p *Proxy) forwardPlainHTTPResponse(
	writer http.ResponseWriter,
	request *http.Request,
	cfg config.MITMConfig,
	provider string,
	upstream string,
	body []byte,
	requestBodyIndex captureBodyIndex,
	streamResp *http.Response,
	plainHTTPSession *livetrack.Session[TunnelMeta],
	started time.Time,
) {
	forwardResponseHeaders(writer.Header(), streamResp.Header)
	writer.WriteHeader(streamResp.StatusCode)
	// Buffer the response up to the store body cap so the full body reaches
	// the capture store while still streaming to the client. The wire-leg
	// summary is derived from this same buffer (summarizeBody previews/caps).
	captureBuffer := &limitedBuffer{limit: p.captureBodyCap(cfg), buf: bytes.Buffer{}}
	counter := &countingWriter{writer: writer, bytesWritten: 0}
	flusher, _ := writer.(http.Flusher)
	copyErr := streamWithFlush(counter, captureBuffer, streamResp.Body, flusher, plainHTTPSession)
	duration := clock.Since(started)
	if copyErr != nil {
		p.log.Warn(
			"mitm.proxy.copy_failed",
			"concern",
			"providers.mitm.wire",
			"provider",
			provider,
			"path",
			request.URL.Path,
			"err",
			copyErr,
		)
	}

	p.finalizePlainHTTPCapture(
		request,
		streamResp,
		plainHTTPCaptureFinalize{
			cfg:              cfg,
			provider:         provider,
			upstream:         upstream,
			requestBody:      body,
			capture:          captureBuffer,
			responseBytes:    counter.bytesWritten,
			requestBodyIndex: requestBodyIndex,
			duration:         duration,
		},
	)
}

func (p *Proxy) readPlainHTTPRequestBody(
	writer http.ResponseWriter,
	request *http.Request,
	cfg config.MITMConfig,
	provider string,
	upstream string,
	started time.Time,
) ([]byte, bool) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.recordPlainHTTPReadFailure(
			request,
			cfg,
			provider,
			upstream+request.URL.RequestURI(),
			started,
			err,
		)
		http.Error(writer, "read request body", http.StatusBadRequest)

		return nil, false
	}

	_ = request.Body.Close()

	return body, true
}

func plainHTTPDispatchInput(
	request *http.Request,
	body []byte,
	provider string,
	upstream string,
) (captureBodyIndex, upstreamRequest) {
	requestBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(body))
	input := upstreamRequest{
		method:   request.Method,
		path:     request.URL.RequestURI(),
		header:   request.Header,
		body:     body,
		provider: provider,
		upstream: upstream,
	}

	return requestBodyIndex, input
}
