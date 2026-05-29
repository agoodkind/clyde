package adapter

import (
	"fmt"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

// adapterRecoveryWriter wraps [http.ResponseWriter] so the adapter boundary
// (Server.handle in handle.go) can detect whether a handler ever committed
// status bytes. The body-not-written backstop relies on wroteHeader, and
// panic recovery uses it to decide whether to write a synthesized error
// envelope or just log.
type adapterRecoveryWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *adapterRecoveryWriter) WriteHeader(statusCode int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *adapterRecoveryWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if err != nil {
		return n, fmt.Errorf("adapter recovery write: %w", err)
	}
	return n, nil
}

func (w *adapterRecoveryWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *adapterRecoveryWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func correlationForRequest(r *http.Request) correlation.Context {
	corr := correlation.FromContext(r.Context())
	if corr.RequestID != "" {
		return corr
	}
	reqID := strings.TrimSpace(r.Header.Get(clydeingress.HeaderRequestID))
	if reqID == "" {
		reqID = newRequestID()
	}
	return clydeingress.FromHTTPHeader(r.Header, reqID)
}
