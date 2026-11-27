package antigravity

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

const (
	captureClient          = "adapter.antigravity"
	captureConcern         = "adapter.antigravity.egress"
	captureResponseBodyCap = 8 << 20
)

type capturedResponseBody struct {
	io.Reader

	body   io.Closer
	record func()
	once   sync.Once
}

func (body *capturedResponseBody) Close() error {
	body.once.Do(body.record)

	if err := body.body.Close(); err != nil {
		slog.Warn("adapter.antigravity.capture_body_close_failed")

		return fmt.Errorf("close captured Code Assist response: %w", err)
	}

	return nil
}

func captureGeneration(
	store *capture.Store,
	corr correlation.Context,
	request *http.Request,
	response *http.Response,
	requestBody []byte,
	sessionID string,
	started time.Time,
) {
	accessToken, _ := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	requestValues, headerValuesComplete := capture.SensitiveHTTPHeaderValuesWithStatus(
		request.Header,
	)
	bodyValues, bodyValuesComplete := capture.SensitiveHTTPBodyValuesWithStatus(requestBody)
	requestValues = append(requestValues, bodyValues...)
	requestHeaders, cleanRequestBody := capture.RedactHTTP(request.Header, requestBody)
	sink := capture.NewCappedBuffer(captureResponseBodyCap)
	response.Body = &capturedResponseBody{
		Reader: io.TeeReader(response.Body, sink),
		body:   response.Body,
		once:   sync.Once{},
		record: func() {
			if !store.RecordingEnabled() {
				return
			}

			responseHeaders, responseBody := capture.RedactHTTPWithSensitiveValuesStatus(
				response.Header,
				sink.Bytes(),
				requestValues,
				headerValuesComplete && bodyValuesComplete,
			)
			redactBearerEcho(responseHeaders, accessToken)
			store.RecordCappedExchange(
				corr,
				capture.Exchange{
					Client:             captureClient,
					Provider:           "antigravity",
					Concern:            captureConcern,
					Host:               request.URL.Host,
					Method:             request.Method,
					Path:               request.URL.Path,
					Status:             response.StatusCode,
					UpstreamRequestID:  responseHeaders.Get("X-Request-ID"),
					SessionID:          sessionID,
					ConversationID:     "",
					ConversationSource: "",
					RequestHeaders:     requestHeaders,
					ResponseHeaders:    responseHeaders,
					RequestBody:        cleanRequestBody,
					ResponseBody:       responseBody,
					RequestType:        request.Header.Get("Content-Type"),
					ResponseType:       responseHeaders.Get("Content-Type"),
					Started:            started,
				},
				sink.TotalRead(),
				sink.Truncated(),
			)
		},
	}
}

func redactBearerEcho(headers http.Header, token string) {
	if token == "" {
		return
	}

	for _, values := range headers {
		for index, value := range values {
			values[index] = strings.ReplaceAll(value, token, "[REDACTED]")
		}
	}
}
