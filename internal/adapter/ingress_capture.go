package adapter

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/gklog/correlation"
)

const (
	// captureClientIngress tags adapter ingress capture rows in capture.db. It
	// is provider-neutral because any OpenAI-compatible client, not only Cursor,
	// reaches this endpoint; the resolved provider rides the egress row.
	captureClientIngress = "adapter.ingress"
	// captureIngressConcern is the capture.db concern tag for ingress rows.
	captureIngressConcern = "adapter.ingress"
)

// ingressCaptureWriter wraps [http.ResponseWriter] to retain the first status
// and a capped copy of the rendered response body, forwarding every call
// (including Flush, which the SSE writer requires) to the embedded writer. It
// mirrors adapterRecoveryWriter so the streaming and error-boundary paths see
// a writer that behaves identically apart from the side-channel capture.
type ingressCaptureWriter struct {
	http.ResponseWriter
	body        *capture.CappedBuffer
	status      int
	wroteHeader bool
}

func newIngressCaptureWriter(inner http.ResponseWriter, capBytes int) *ingressCaptureWriter {
	return &ingressCaptureWriter{
		ResponseWriter: inner,
		body:           capture.NewCappedBuffer(capBytes),
		status:         0,
		wroteHeader:    false,
	}
}

func (w *ingressCaptureWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *ingressCaptureWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(p)
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("ingress capture write: %w", err)
	}
	return n, nil
}

// Flush commits a 200 if nothing wrote yet, then forwards to the embedded
// [http.Flusher]. The SSE writer asserts [http.Flusher] on the response writer,
// so this method is required or every streaming chat fails at SSE construction.
func (w *ingressCaptureWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the wrapped writer so the error boundary can reach it.
func (w *ingressCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// beginIngressCapture wraps hctx.Writer when the capture store is open and the
// ingress knob is on, returning the wrapper. It returns nil when capture is
// off, in which case nothing is wrapped and there is no added cost.
func (s *Server) beginIngressCapture(hctx *handlerCtx) *ingressCaptureWriter {
	if s.deps.CaptureStore == nil || !s.cfg.CaptureIngress {
		return nil
	}
	capW := newIngressCaptureWriter(hctx.Writer, capture.DefaultMaxBodyBytes)
	hctx.Writer = capW
	return capW
}

// finishIngressCapture records the ingress exchange. A successful or mid-stream
// response carries the status and rendered body the wrapper observed. A request
// that errored before any write carries the status of the returned
// adapterError and an empty response body, because the error envelope is
// rendered downstream on the raw writer rather than through the wrapper.
// Credential and account values are removed from both captured stages.
func (s *Server) finishIngressCapture(capW *ingressCaptureWriter, corr correlation.Context, r *http.Request, body []byte, started time.Time, handlerErr error) {
	if capW == nil {
		return
	}
	status := capW.status
	if status == 0 && handlerErr != nil {
		status = http.StatusInternalServerError
		var aerr *adapterError
		if errors.As(handlerErr, &aerr) && aerr.HTTPStatus != 0 {
			status = aerr.HTTPStatus
		}
	}
	conversationID, conversationSource := ingressConversationFields(corr)
	requestHeaders, requestBody := capture.RedactHTTP(r.Header, body)
	responseHeaders, responseBody := capture.RedactHTTP(capW.Header(), capW.body.Bytes())
	s.deps.CaptureStore.RecordExchange(corr, capture.Exchange{
		Client:             captureClientIngress,
		Provider:           "",
		Concern:            captureIngressConcern,
		Host:               r.Host,
		Method:             r.Method,
		Path:               r.URL.Path,
		Status:             status,
		UpstreamRequestID:  "",
		SessionID:          "",
		ConversationID:     conversationID,
		ConversationSource: conversationSource,
		RequestHeaders:     requestHeaders,
		ResponseHeaders:    responseHeaders,
		RequestBody:        requestBody,
		ResponseBody:       responseBody,
		RequestType:        r.Header.Get("Content-Type"),
		ResponseType:       capW.Header().Get("Content-Type"),
		Started:            started,
	})
}

// ingressConversationFields derives the capture row's conversation identity
// from the request's chat identity.
//
// Only a native chat key becomes a conversation id. Cursor supplies one when it
// sends conversation metadata; otherwise the resolver derives a lineage key
// that groups related requests but names no conversation. Storing a derived key
// here would put a value in conversation_id that
// `clyde conversation export <id>` cannot resolve, so a derived key is left
// out and the column keeps its single meaning.
func ingressConversationFields(corr correlation.Context) (conversationID string, conversationSource string) {
	if clydeingress.ChatKeySource(corr) != clydeingress.ChatKeySourceNative {
		return "", ""
	}
	nativeID := strings.TrimSpace(clydeingress.ChatKey(corr))
	if nativeID == "" {
		return "", ""
	}
	return conversation.DerivedID(providerid.ProviderCursor, nativeID, ""), clydeingress.ChatKeySourceNative
}

func sanitizedCaptureResponseHeaders(headers http.Header) http.Header {
	sanitized := headers.Clone()
	for name := range sanitized {
		if redactedHeader(strings.ToLower(name)) {
			sanitized.Del(name)
		}
	}
	return sanitized
}
