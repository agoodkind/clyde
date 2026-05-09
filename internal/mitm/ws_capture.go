package mitm

import (
	"crypto/tls"
	"errors"
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
	upstreamURL := wsUpstreamURL(upstream, r.URL.RequestURI())
	upstreamHeaders := wsUpstreamHeaders(r.Header)

	upstreamConn, upstreamRespHeaders, err := p.dialWSUpstream(w, r, upstreamURL, upstreamHeaders)
	if err != nil {
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

	if !p.recordWSStart(cfg.CaptureDir, capturePolicy, provider, upstreamURL, r.Header, upstreamRespHeaders, corr) {
		return
	}

	state := &wsRelayState{
		mu:           sync.Mutex{},
		messageCount: 0,
		closeOnce:    sync.Once{},
		closeErr:     nil,
		closeChan:    make(chan struct{}),
	}
	closeBoth := wsCloseBoth(state, clientConn, upstreamConn)
	relay := p.wsMakeRelay(wsRelayParams{
		state:         state,
		closeBoth:     closeBoth,
		provider:      provider,
		upstreamURL:   upstreamURL,
		captureDir:    cfg.CaptureDir,
		capturePolicy: capturePolicy,
		corr:          corr,
	})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.wsHandleRelayPanic("mitm.ws.client_relay_panic", upstreamURL, fmt.Sprintf("%v", recovered), closeBoth)
			}
		}()
		relay(clientConn, upstreamConn, true)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.wsHandleRelayPanic("mitm.ws.upstream_relay_panic", upstreamURL, fmt.Sprintf("%v", recovered), closeBoth)
			}
		}()
		relay(upstreamConn, clientConn, false)
	}()

	<-state.closeChan

	p.recordWSEnd(cfg.CaptureDir, capturePolicy, provider, upstreamURL, state.messageCount, corr, state.closeErr)
	queueBaselineRefresh(r.Context(), cfg, provider, p.log)
	p.log.Info("mitm.ws.closed", "url", upstreamURL, "messages", state.messageCount)
}

// wsRelayState holds the shared mutable state across the two relay
// goroutines that bridge a websocket session.
type wsRelayState struct {
	mu           sync.Mutex
	messageCount int
	closeOnce    sync.Once
	closeErr     error
	closeChan    chan struct{}
}

func wsCloseBoth(state *wsRelayState, clientConn, upstreamConn *websocket.Conn) func(error) {
	return func(reason error) {
		state.closeOnce.Do(func() {
			state.closeErr = reason
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			close(state.closeChan)
		})
	}
}

type wsRelayParams struct {
	state         *wsRelayState
	closeBoth     func(error)
	provider      string
	upstreamURL   string
	captureDir    string
	capturePolicy CaptureFilePolicy
	corr          correlation.Context
}

// wsMakeRelay returns the per-direction relay loop. It reads frames
// from src, mirrors them to dst, and records each frame to the JSONL
// capture stream. On any read, write, or capture failure it triggers
// the shared closeBoth so the partner direction also exits.
func (p *Proxy) wsMakeRelay(params wsRelayParams) func(src, dst *websocket.Conn, fromClient bool) {
	return func(src, dst *websocket.Conn, fromClient bool) {
		for {
			messageType, payload, err := src.ReadMessage()
			if err != nil {
				params.closeBoth(err)
				return
			}
			params.state.mu.Lock()
			params.state.messageCount++
			count := params.state.messageCount
			params.state.mu.Unlock()
			text := ""
			if messageType == websocket.TextMessage {
				text = string(payload)
			}
			ev := map[string]any{
				"provider":    params.provider,
				"kind":        "ws_msg",
				"t":           currentTime().Unix(),
				"url":         params.upstreamURL,
				"from_client": fromClient,
				"len":         len(payload),
				"text":        text,
				"seq":         count,
			}
			addCaptureCorrelation(ev, params.corr)
			if err := p.recordWSMessage(params.captureDir, ev, params.capturePolicy); err != nil {
				params.closeBoth(err)
				return
			}
			if err := dst.WriteMessage(messageType, payload); err != nil {
				params.closeBoth(err)
				return
			}
		}
	}
}

// wsHandleRelayPanic logs a recovered panic from one of the relay
// goroutines and triggers closeBoth so the partner direction also
// exits. The recover() itself stays in the goroutine literal so the
// staticcheck-extra goroutine_without_recover rule sees it; the
// recovered value is stringified at the goroutine boundary so this
// signature stays free of any.
func (p *Proxy) wsHandleRelayPanic(panicEvent string, upstreamURL string, recoveredString string, closeBoth func(error)) {
	p.log.Error(panicEvent,
		"component", "mitm",
		"concern", "providers.mitm.lifecycle",
		"url", upstreamURL,
		"err", fmt.Errorf("panic: %s", recoveredString),
	)
	closeBoth(fmt.Errorf("relay panic: %s", recoveredString))
}

// wsUpstreamURL converts an https/http base into a wss/ws URL with
// the original request URI appended.
func wsUpstreamURL(upstream string, requestURI string) string {
	url := upstream + requestURI
	switch {
	case strings.HasPrefix(url, "https://"):
		return "wss://" + strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		return "ws://" + strings.TrimPrefix(url, "http://")
	}
	return url
}

// wsUpstreamHeaders forwards all client headers to the upstream
// handshake except the ws control headers gorilla/websocket sets
// itself. The websocket library rejects requests carrying these.
func wsUpstreamHeaders(src http.Header) http.Header {
	out := http.Header{}
	for key, values := range src {
		switch strings.ToLower(key) {
		case "upgrade", "connection", "sec-websocket-key",
			"sec-websocket-version", "sec-websocket-extensions",
			"sec-websocket-protocol":
			continue
		}
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

// dialWSUpstream dials the upstream websocket. On failure it writes
// a 502 to the client, logs the failure, and closes the handshake
// response body. On success it returns the upstream conn and the
// handshake response headers; the response body is empty for a
// successful websocket upgrade so we do not return it to the caller.
func (p *Proxy) dialWSUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, headers http.Header) (*websocket.Conn, http.Header, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		TLSClientConfig:  &tls.Config{},
	}
	upstreamConn, upstreamResp, err := dialer.DialContext(r.Context(), upstreamURL, headers)
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
		return nil, nil, fmt.Errorf("dial websocket upstream: %w", err)
	}
	headersCopy := upstreamResp.Header.Clone()
	if upstreamResp.Body != nil {
		_ = upstreamResp.Body.Close()
	}
	return upstreamConn, headersCopy, nil
}

// recordWSStart writes the ws_start capture event. Returns false if
// the capture sink is already closed so the caller aborts the
// websocket session early instead of relaying frames whose captures
// would all fail.
func (p *Proxy) recordWSStart(captureDir string, policy CaptureFilePolicy, provider string, upstreamURL string, requestHeaders http.Header, responseHeaders http.Header, corr correlation.Context) bool {
	startEvent := map[string]any{
		"provider":         provider,
		"kind":             "ws_start",
		"t":                currentTime().Unix(),
		"url":              upstreamURL,
		"request_headers":  redactHeaders(requestHeaders),
		"response_headers": redactHeaders(responseHeaders),
	}
	addCaptureCorrelation(startEvent, corr)
	err := p.writeCaptureEvent(captureDir, startEvent, policy)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrCaptureSinkClosed) {
		p.log.Debug("mitm.ws.capture_skipped_closed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
		)
		return false
	}
	p.log.Warn("mitm.ws.capture_start_failed",
		"component", "mitm",
		"concern", "providers.mitm.wire",
		"err", err,
	)
	return true
}

// recordWSMessage writes a single ws_msg capture event. Returns
// ErrCaptureSinkClosed if the cache is closed so the caller can tear
// down the relay; other errors are logged and swallowed because a
// single capture failure must not break the proxied session.
func (p *Proxy) recordWSMessage(captureDir string, ev map[string]any, policy CaptureFilePolicy) error {
	err := p.writeCaptureEvent(captureDir, ev, policy)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCaptureSinkClosed) {
		return err
	}
	p.log.Warn("mitm.ws.capture_msg_failed",
		"component", "mitm",
		"concern", "providers.mitm.wire",
		"err", err,
	)
	return nil
}

// recordWSEnd writes the terminal ws_end capture event.
func (p *Proxy) recordWSEnd(captureDir string, policy CaptureFilePolicy, provider string, upstreamURL string, messageCount int, corr correlation.Context, closeErr error) {
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
	err := p.writeCaptureEvent(captureDir, endEvent, policy)
	if err == nil {
		return
	}
	if errors.Is(err, ErrCaptureSinkClosed) {
		p.log.Debug("mitm.ws.capture_end_skipped_closed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
		)
		return
	}
	p.log.Warn("mitm.ws.capture_end_failed",
		"component", "mitm",
		"concern", "providers.mitm.wire",
		"err", err,
	)
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
