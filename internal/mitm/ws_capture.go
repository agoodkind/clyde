package mitm

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goodkind.io/clyde/internal/correlation"
)

// isWebsocketUpgrade reports whether the request asks for a
// websocket protocol upgrade. Matching is case-insensitive on the
// "Connection: upgrade" and "Upgrade: websocket" headers.
func isWebsocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				return true
			}
		}
	}
	return false
}

// handleWebsocket bridges a client websocket connection to the
// upstream and records every text frame to the JSONL capture
// stream. The schema matches the dump.py mitmproxy addon under
// research/codex/captures/2026-04-27/ so existing captures and new
// captures slot into the same toolchain.
//
// On error at any stage we close both ends and emit a ws_end record
// with the error message in the close field.
func (p *Proxy) handleWebsocket(w http.ResponseWriter, r *http.Request, provider string, upstream string) {
	cfg := p.config()
	capturePolicy := captureFilePolicyFromConfig(cfg)

	// Build the upstream URL in ws scheme.
	upstreamURL := upstream + r.URL.RequestURI()
	switch {
	case strings.HasPrefix(upstreamURL, "https://"):
		upstreamURL = "wss://" + strings.TrimPrefix(upstreamURL, "https://")
	case strings.HasPrefix(upstreamURL, "http://"):
		upstreamURL = "ws://" + strings.TrimPrefix(upstreamURL, "http://")
	}

	// Forward all client headers to the upstream handshake except
	// the ws control headers gorilla/websocket sets itself. The
	// websocket library rejects requests carrying these.
	upstreamHeaders := http.Header{}
	for key, values := range r.Header {
		switch strings.ToLower(key) {
		case "upgrade", "connection", "sec-websocket-key",
			"sec-websocket-version", "sec-websocket-extensions",
			"sec-websocket-protocol":
			continue
		}
		for _, value := range values {
			upstreamHeaders.Add(key, value)
		}
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		TLSClientConfig:  &tls.Config{},
	}
	upstreamConn, upstreamResp, err := dialer.DialContext(r.Context(), upstreamURL, upstreamHeaders)
	if err != nil {
		status := http.StatusBadGateway
		if upstreamResp != nil {
			status = upstreamResp.StatusCode
			if upstreamResp.Body != nil {
				_ = upstreamResp.Body.Close()
			}
		}
		p.log.Warn("mitm.ws.dial_failed", "url", upstreamURL, "status", status, "err", err)
		http.Error(w, "ws upstream dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	upgrader := websocket.Upgrader{
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: false,
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.log.Warn("mitm.ws.upgrade_failed", "err", err)
		return
	}
	defer func() { _ = clientConn.Close() }()
	corr := correlation.FromHTTPHeader(r.Header, r.Header.Get(correlation.HeaderRequestID))

	startEvent := map[string]any{
		"provider":         provider,
		"kind":             "ws_start",
		"t":                currentTime().Unix(),
		"url":              upstreamURL,
		"request_headers":  redactHeaders(r.Header),
		"response_headers": redactHeaders(upstreamResp.Header),
	}
	addCaptureCorrelation(startEvent, corr)
	if err := WriteCaptureEvent(cfg.CaptureDir, startEvent, capturePolicy); err != nil {
		p.log.Warn("mitm.ws.capture_start_failed", "err", err)
	}

	var (
		messageCount int
		messageMu    sync.Mutex
		closeOnce    sync.Once
		closeErr     error
		closeChan    = make(chan struct{})
		captureDir   = cfg.CaptureDir
	)

	closeBoth := func(reason error) {
		closeOnce.Do(func() {
			closeErr = reason
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			close(closeChan)
		})
	}

	relay := func(src, dst *websocket.Conn, fromClient bool) {
		for {
			messageType, payload, err := src.ReadMessage()
			if err != nil {
				closeBoth(err)
				return
			}
			messageMu.Lock()
			messageCount++
			count := messageCount
			messageMu.Unlock()
			text := ""
			if messageType == websocket.TextMessage {
				text = string(payload)
			}
			ev := map[string]any{
				"provider":    provider,
				"kind":        "ws_msg",
				"t":           currentTime().Unix(),
				"url":         upstreamURL,
				"from_client": fromClient,
				"len":         len(payload),
				"text":        text,
				"seq":         count,
			}
			addCaptureCorrelation(ev, corr)
			if err := WriteCaptureEvent(captureDir, ev, capturePolicy); err != nil {
				p.log.Warn("mitm.ws.capture_msg_failed", "err", err)
			}
			if err := dst.WriteMessage(messageType, payload); err != nil {
				closeBoth(err)
				return
			}
		}
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.Error("mitm.ws.client_relay_panic",
					"url", upstreamURL,
					"err", fmt.Errorf("panic: %v", recovered),
				)
				closeBoth(fmt.Errorf("client relay panic: %v", recovered))
			}
		}()
		relay(clientConn, upstreamConn, true)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.Error("mitm.ws.upstream_relay_panic",
					"url", upstreamURL,
					"err", fmt.Errorf("panic: %v", recovered),
				)
				closeBoth(fmt.Errorf("upstream relay panic: %v", recovered))
			}
		}()
		relay(upstreamConn, clientConn, false)
	}()

	<-closeChan

	endEvent := map[string]any{
		"provider": provider,
		"kind":     "ws_end",
		"t":        currentTime().Unix(),
		"url":      upstreamURL,
		"messages": messageCount,
	}
	addCaptureCorrelation(endEvent, corr)
	if closeErr != nil {
		endEvent["err"] = closeErr.Error()
	}
	if err := WriteCaptureEvent(captureDir, endEvent, capturePolicy); err != nil {
		p.log.Warn("mitm.ws.capture_end_failed", "err", err)
	}
	queueBaselineRefresh(cfg, provider, p.log)
	p.log.Info("mitm.ws.closed", "url", upstreamURL, "messages", messageCount)
}

func addCaptureCorrelation(event map[string]any, corr correlation.Context) {
	if corr.TraceID != "" {
		event["trace_id"] = string(corr.TraceID)
	}
	if corr.SpanID != "" {
		event["span_id"] = string(corr.SpanID)
	}
	if corr.ParentSpanID != "" {
		event["parent_span_id"] = string(corr.ParentSpanID)
	}
	if corr.RequestID != "" {
		event["request_id"] = corr.RequestID
	}
	if corr.CursorRequestID != "" {
		event["cursor_request_id"] = corr.CursorRequestID
	}
	if corr.CursorConversationID != "" {
		event["cursor_conversation_id"] = corr.CursorConversationID
	}
	if corr.UpstreamRequestID != "" {
		event["upstream_request_id"] = corr.UpstreamRequestID
	}
	if corr.UpstreamResponseID != "" {
		event["upstream_response_id"] = corr.UpstreamResponseID
	}
}
