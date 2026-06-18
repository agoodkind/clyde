package mitm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
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
// upstream. Bridged frames accumulate into the SQLite capture store
// (client frames as the request body, upstream frames as the response
// body); the wire JSONL receives only lifecycle legs and a terminal
// capture-index leg carrying the close status, close reason, and message
// count, never frame content.
//
// On error at any stage we close both ends and emit the terminal
// capture-index leg with the error message in the close field.
func (p *Proxy) handleWebsocket(w http.ResponseWriter, r *http.Request, provider string, upstream string) {
	cfg := p.config()
	started := clock.Now()
	upstreamURL := wsUpstreamURL(upstream, r.URL.RequestURI())
	upstreamHeaders := wsUpstreamHeaders(r.Header)
	requestContentType := r.Header.Get("Content-Type")
	recorder := p.beginWSLogRecorder(r, provider, upstreamURL)
	captureStoreRecorder := newWSCaptureStoreRecorder(p.captureBodyCap(cfg))
	clientFacet := extractIdentityContribution(r.Host, r.URL.Path, r.Header).Facet
	ctx := r.Context()

	p.emitWSInitialLogLegs(ctx, recorder, cfg.CaptureDir, requestContentType, clientFacet)

	upstreamConn, upstreamRespHeaders, upstreamStatus, err := p.dialWSUpstream(w, r, upstreamURL, upstreamHeaders)
	if err != nil {
		p.recordWSFailure(ctx, recorder, "ws_upstream_dial_failed", err.Error())
		return
	}
	defer func() { _ = upstreamConn.Close() }()
	responseContentType := upstreamRespHeaders.Get("Content-Type")
	p.emitWSUpstreamStartLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, upstreamStatus, clientFacet)

	upgrader := websocket.Upgrader{
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: false,
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.log.Warn("mitm.ws.upgrade_failed", "concern", "providers.mitm.wire", "err", err)
		p.recordWSFailure(ctx, recorder, "ws_client_upgrade_failed", err.Error())
		return
	}
	defer func() { _ = clientConn.Close() }()

	state := &wsRelayState{
		mu:           sync.Mutex{},
		messageCount: 0,
		closeOnce:    sync.Once{},
		closeErr:     nil,
		closeChan:    make(chan struct{}),
	}
	closeBoth := wsCloseBoth(state, clientConn, upstreamConn)
	relay := p.wsMakeRelay(wsRelayParams{
		state:                state,
		closeBoth:            closeBoth,
		recorder:             recorder,
		captureDir:           cfg.CaptureDir,
		captureStoreRecorder: captureStoreRecorder,
		session:              nil,
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

	p.emitWSForwardLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, clientFacet)
	p.recordWSEnd(ctx, recorder, cfg.CaptureDir, state.messageCount, state.closeErr)
	p.emitWSCompleteLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, clientFacet)
	p.recordWSCaptureStore(r, upstreamRespHeaders, wsCaptureStoreInput{
		recorder: captureStoreRecorder,
		provider: provider,
		host:     r.Host,
		status:   upstreamStatus,
		started:  started,
	})
	if recorder != nil {
		recorder.Complete(ctx)
	}
	queueBaselineRefresh(ctx, cfg, provider, p.log)
	p.log.Info("mitm.ws.closed", "concern", "providers.mitm.wire", "url", upstreamURL, "messages", state.messageCount)
}

func (p *Proxy) handleProviderInterceptedWebsocket(ctx context.Context, client net.Conn, reader *bufio.Reader, writer *bufio.Writer, r *http.Request, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta]) error {
	cfg := p.config()
	started := clock.Now()
	providerID := provider.ID().String()
	upstreamURL := wsUpstreamURL("https://"+target, r.URL.RequestURI())
	upstreamHeaders := wsUpstreamHeaders(r.Header)
	requestContentType := r.Header.Get("Content-Type")
	recorder := p.beginWSLogRecorder(r, providerID, upstreamURL)
	captureStoreRecorder := newWSCaptureStoreRecorder(p.captureBodyCap(cfg))
	clientFacet := provider.ExtractIdentity(r.Header).Facet

	p.emitWSInitialLogLegs(ctx, recorder, cfg.CaptureDir, requestContentType, clientFacet)

	upstreamConn, upstreamRespHeaders, upstreamStatus, err := p.dialWSUpstreamContext(ctx, upstreamURL, upstreamHeaders)
	if err != nil {
		_ = p.writeInterceptedHTTPError(ctx, writer, http.StatusBadGateway, "ws upstream dial failed: "+err.Error())
		p.recordWSFailure(ctx, recorder, "ws_upstream_dial_failed", err.Error())
		return fmt.Errorf("dial intercepted websocket upstream %s: %w", upstreamURL, err)
	}
	defer func() { _ = upstreamConn.Close() }()
	responseContentType := upstreamRespHeaders.Get("Content-Type")
	p.emitWSUpstreamStartLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, upstreamStatus, clientFacet)

	responseWriter := &interceptedWebsocketResponseWriter{
		header:      http.Header{},
		conn:        client,
		reader:      reader,
		writer:      writer,
		status:      0,
		wroteHeader: false,
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: false,
	}
	clientConn, err := upgrader.Upgrade(responseWriter, r, nil)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.ws.upgrade_failed", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", err)
		p.recordWSFailure(ctx, recorder, "ws_client_upgrade_failed", err.Error())
		return fmt.Errorf("upgrade intercepted websocket client: %w", err)
	}
	defer func() { _ = clientConn.Close() }()

	state := &wsRelayState{
		mu:           sync.Mutex{},
		messageCount: 0,
		closeOnce:    sync.Once{},
		closeErr:     nil,
		closeChan:    make(chan struct{}),
	}
	closeBoth := wsCloseBoth(state, clientConn, upstreamConn)
	relay := p.wsMakeRelay(wsRelayParams{
		state:                state,
		closeBoth:            closeBoth,
		recorder:             recorder,
		captureDir:           cfg.CaptureDir,
		captureStoreRecorder: captureStoreRecorder,
		session:              parent,
	})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.wsHandleRelayPanic("mitm.provider.ws.client_relay_panic", upstreamURL, fmt.Sprintf("%v", recovered), closeBoth)
			}
		}()
		relay(clientConn, upstreamConn, true)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.wsHandleRelayPanic("mitm.provider.ws.upstream_relay_panic", upstreamURL, fmt.Sprintf("%v", recovered), closeBoth)
			}
		}()
		relay(upstreamConn, clientConn, false)
	}()

	<-state.closeChan

	p.emitWSForwardLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, clientFacet)
	p.recordWSEnd(ctx, recorder, cfg.CaptureDir, state.messageCount, state.closeErr)
	p.emitWSCompleteLogLeg(ctx, recorder, cfg.CaptureDir, requestContentType, responseContentType, clientFacet)
	p.recordWSCaptureStore(r, upstreamRespHeaders, wsCaptureStoreInput{
		recorder: captureStoreRecorder,
		provider: providerID,
		host:     host,
		status:   upstreamStatus,
		started:  started,
	})
	if recorder != nil {
		recorder.Complete(ctx)
	}
	queueBaselineRefresh(ctx, cfg, providerID, p.log)
	p.log.InfoContext(ctx, "mitm.provider.ws.closed", "concern", "providers.mitm.wire", "provider", providerID, "url", upstreamURL, "messages", state.messageCount)
	return nil
}

func (p *Proxy) emitWSInitialLogLegs(ctx context.Context, recorder *logevent.Recorder, captureDir string, requestContentType string, clientFacet logevent.Facet) {
	input := newWSLogLegInput(captureDir, requestContentType, clientFacet)
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMIngress, logevent.PhaseStarted, input)
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMPayload, logevent.PhaseCompleted, input)
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMUpstreamSend, logevent.PhaseStarted, input)
}

func (p *Proxy) emitWSUpstreamStartLogLeg(ctx context.Context, recorder *logevent.Recorder, captureDir string, requestContentType string, responseContentType string, statusCode int, clientFacet logevent.Facet) {
	input := newWSLogLegInput(captureDir, requestContentType, clientFacet)
	input.ResponseContentType = responseContentType
	input.StatusCode = statusCode
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMUpstreamStart, logevent.PhaseCompleted, input)
}

func (p *Proxy) emitWSForwardLogLeg(ctx context.Context, recorder *logevent.Recorder, captureDir string, requestContentType string, responseContentType string, clientFacet logevent.Facet) {
	input := newWSLogLegInput(captureDir, requestContentType, clientFacet)
	input.ResponseContentType = responseContentType
	input.StatusCode = http.StatusSwitchingProtocols
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMForward, logevent.PhaseCompleted, input)
}

func (p *Proxy) emitWSCompleteLogLeg(ctx context.Context, recorder *logevent.Recorder, captureDir string, requestContentType string, responseContentType string, clientFacet logevent.Facet) {
	input := newWSLogLegInput(captureDir, requestContentType, clientFacet)
	input.ResponseContentType = responseContentType
	input.StatusCode = http.StatusSwitchingProtocols
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMComplete, logevent.PhaseCompleted, input)
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
	state                *wsRelayState
	closeBoth            func(error)
	recorder             *logevent.Recorder
	captureDir           string
	captureStoreRecorder *wsCaptureStoreRecorder
	// session, when non-nil, has its activity timestamp refreshed
	// after every successful ReadMessage and WriteMessage so the
	// daemon reload-drain idle-grace fast-path can tell an actively
	// streaming websocket from a wedged one. Plain (non-intercepted)
	// websocket sessions never register with livetrack and pass nil
	// here.
	session *livetrack.Session[TunnelMeta]
}

// wsMakeRelay returns the per-direction relay loop. It reads frames
// from src, mirrors them to dst, and accumulates each bridged frame
// into the SQLite capture-store recorder. Frame content is never
// written to the wire JSONL; the single capture-index leg emitted at
// session close carries only a content-free per-direction summary. On
// any read or write failure it triggers the shared closeBoth so the
// partner direction also exits.
//
// When params.session is non-nil, each successful ReadMessage and
// WriteMessage refreshes the session's activity timestamp so the
// daemon reload-drain idle-grace fast-path can distinguish active
// streaming bridges from wedged keepalive ones.
func (p *Proxy) wsMakeRelay(params wsRelayParams) func(src, dst *websocket.Conn, fromClient bool) {
	return func(src, dst *websocket.Conn, fromClient bool) {
		for {
			messageType, payload, err := src.ReadMessage()
			if err != nil {
				params.closeBoth(err)
				return
			}
			params.session.Touch()
			params.state.mu.Lock()
			params.state.messageCount++
			params.state.mu.Unlock()
			if err := dst.WriteMessage(messageType, payload); err != nil {
				params.closeBoth(err)
				return
			}
			params.captureStoreRecorder.add(payload, fromClient)
			params.session.Touch()
		}
	}
}

type wsCaptureStoreInput struct {
	recorder *wsCaptureStoreRecorder
	provider string
	host     string
	status   int
	started  time.Time
}

func (p *Proxy) recordWSCaptureStore(r *http.Request, responseHeader http.Header, in wsCaptureStoreInput) {
	requestBody, responseBody := in.recorder.bodies()
	p.recordCaptureStore(r, responseHeader, captureStoreInput{
		provider:     in.provider,
		host:         in.host,
		method:       "WEBSOCKET",
		path:         r.URL.Path,
		status:       in.status,
		requestBody:  requestBody,
		responseBody: responseBody,
		duration:     clock.Since(in.started),
	})
}

// wsCaptureStoreRecorder accumulates bridged websocket payloads for one
// capture-store row. It mirrors the adapter Codex websocket capture shape:
// client frames become the request body and upstream frames become the response
// body, each newline-delimited and capped before persistence.
type wsCaptureStoreRecorder struct {
	mu       sync.Mutex
	request  bytes.Buffer
	response bytes.Buffer
	capBytes int
}

func newWSCaptureStoreRecorder(capBytes int) *wsCaptureStoreRecorder {
	if capBytes <= 0 {
		capBytes = defaultCaptureBodyCap
	}
	return &wsCaptureStoreRecorder{
		mu:       sync.Mutex{},
		request:  bytes.Buffer{},
		response: bytes.Buffer{},
		capBytes: capBytes,
	}
}

func (r *wsCaptureStoreRecorder) add(payload []byte, fromClient bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fromClient {
		appendCappedWSFrame(&r.request, payload, r.capBytes)
		return
	}
	appendCappedWSFrame(&r.response, payload, r.capBytes)
}

func appendCappedWSFrame(dst *bytes.Buffer, payload []byte, capBytes int) {
	if dst.Len() >= capBytes {
		return
	}
	if dst.Len() > 0 {
		if dst.Len()+1 > capBytes {
			return
		}
		dst.WriteByte('\n')
	}
	remaining := capBytes - dst.Len()
	if len(payload) > remaining {
		payload = payload[:remaining]
	}
	dst.Write(payload)
}

func (r *wsCaptureStoreRecorder) bodies() ([]byte, []byte) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	requestBody := append([]byte(nil), r.request.Bytes()...)
	responseBody := append([]byte(nil), r.response.Bytes()...)
	return requestBody, responseBody
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
// wsHandshakeReservedHeader enumerates the websocket control headers
// that gorilla/websocket sets itself; forwarding any of them on the
// upstream handshake gets the upgrade rejected.
type wsHandshakeReservedHeader string

const (
	wsHandshakeUpgrade                wsHandshakeReservedHeader = "upgrade"
	wsHandshakeConnection             wsHandshakeReservedHeader = "connection"
	wsHandshakeSecWebsocketKey        wsHandshakeReservedHeader = "sec-websocket-key"
	wsHandshakeSecWebsocketVersion    wsHandshakeReservedHeader = "sec-websocket-version"
	wsHandshakeSecWebsocketExtensions wsHandshakeReservedHeader = "sec-websocket-extensions"
	wsHandshakeSecWebsocketProtocol   wsHandshakeReservedHeader = "sec-websocket-protocol"
)

func wsUpstreamHeaders(src http.Header) http.Header {
	out := http.Header{}
	for key, values := range src {
		switch wsHandshakeReservedHeader(strings.ToLower(key)) {
		case wsHandshakeUpgrade, wsHandshakeConnection, wsHandshakeSecWebsocketKey,
			wsHandshakeSecWebsocketVersion, wsHandshakeSecWebsocketExtensions,
			wsHandshakeSecWebsocketProtocol:
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
func (p *Proxy) dialWSUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, headers http.Header) (*websocket.Conn, http.Header, int, error) {
	upstreamConn, headersCopy, status, err := p.dialWSUpstreamContext(r.Context(), upstreamURL, headers)
	if err == nil {
		return upstreamConn, headersCopy, status, nil
	}
	p.log.Warn("mitm.ws.dial_failed", "concern", "providers.mitm.wire", "url", upstreamURL, "status", status, "err", err)
	http.Error(w, "ws upstream dial failed: "+err.Error(), http.StatusBadGateway)
	return nil, nil, status, fmt.Errorf("dial websocket upstream: %w", err)
}

func (p *Proxy) dialWSUpstreamContext(ctx context.Context, upstreamURL string, headers http.Header) (*websocket.Conn, http.Header, int, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		NetDialContext:   p.dialContext,
		TLSClientConfig:  p.tlsClientConfig,
	}
	upstreamConn, upstreamResp, err := dialer.DialContext(ctx, upstreamURL, headers)
	if err != nil {
		status := http.StatusBadGateway
		if upstreamResp != nil {
			status = upstreamResp.StatusCode
			if upstreamResp.Body != nil {
				_ = upstreamResp.Body.Close()
			}
		}
		p.log.WarnContext(ctx, "mitm.ws.upstream_dial_failed", "concern", "providers.mitm.wire", "url", upstreamURL, "status", status, "err", err)
		return nil, nil, status, fmt.Errorf("dial websocket upstream: %w", err)
	}
	headersCopy := upstreamResp.Header.Clone()
	if upstreamResp.Body != nil {
		_ = upstreamResp.Body.Close()
	}
	return upstreamConn, headersCopy, http.StatusSwitchingProtocols, nil
}

type interceptedWebsocketResponseWriter struct {
	header      http.Header
	conn        net.Conn
	reader      *bufio.Reader
	writer      *bufio.Writer
	status      int
	wroteHeader bool
}

func (w *interceptedWebsocketResponseWriter) Header() http.Header {
	return w.header
}

func (w *interceptedWebsocketResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "status"
	}
	_, _ = fmt.Fprintf(w.writer, "HTTP/1.1 %d %s\r\n", status, statusText)
	for key, values := range w.header {
		for _, value := range values {
			_, _ = fmt.Fprintf(w.writer, "%s: %s\r\n", key, value)
		}
	}
	_, _ = w.writer.WriteString("\r\n")
	_ = w.writer.Flush()
}

func (w *interceptedWebsocketResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.writer.Write(body)
	if err != nil {
		return written, fmt.Errorf("write intercepted websocket response body: %w", err)
	}
	if err := w.writer.Flush(); err != nil {
		return written, fmt.Errorf("flush intercepted websocket response body: %w", err)
	}
	return written, nil
}

func (w *interceptedWebsocketResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(w.reader, w.writer), nil
}

func (p *Proxy) writeInterceptedHTTPError(ctx context.Context, writer *bufio.Writer, status int, body string) error {
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "status"
	}
	_, err := fmt.Fprintf(
		writer,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s",
		status,
		statusText,
		len(body),
		body,
	)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.write_error_response_failed", "concern", "providers.mitm.wire", "status", status, "err", err)
		return fmt.Errorf("write intercepted HTTP error response: %w", err)
	}
	if err := writer.Flush(); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.flush_error_response_failed", "concern", "providers.mitm.wire", "status", status, "err", err)
		return fmt.Errorf("flush intercepted HTTP error response: %w", err)
	}
	return nil
}

func (p *Proxy) beginWSLogRecorder(r *http.Request, provider string, upstreamURL string) *logevent.Recorder {
	var path logevent.Path
	path.Surface = logevent.SurfaceMITMIDE
	path.RouteFamily = logevent.RouteFamilyMITMProxy
	path.Path = r.URL.Path
	path.Method = r.Method
	path.Host = r.Host
	path.Provider = provider
	path.UpstreamURL = upstreamURL
	if parsedURL, err := url.Parse(upstreamURL); err == nil {
		path.Path = parsedURL.Path
		path.Host = parsedURL.Host
	}
	return p.beginMITMLogRecorder(r.Header, path)
}

type wsLogLegInput struct {
	CaptureDir          string
	RequestContentType  string
	ResponseContentType string
	Status              logevent.Status
	StatusCode          int
	ErrorCode           string
	ErrorMessage        string
	BytesIn             int64
	BytesOut            int64
	Direction           string
	Sequence            int
	CloseReason         string
	ClientFacet         logevent.Facet
}

func newWSLogLegInput(captureDir string, requestContentType string, clientFacet logevent.Facet) wsLogLegInput {
	return wsLogLegInput{
		CaptureDir:          captureDir,
		RequestContentType:  requestContentType,
		ResponseContentType: "",
		Status:              "",
		StatusCode:          0,
		ErrorCode:           "",
		ErrorMessage:        "",
		BytesIn:             0,
		BytesOut:            0,
		Direction:           "",
		Sequence:            0,
		CloseReason:         "",
		ClientFacet:         clientFacet,
	}
}

func (p *Proxy) emitWSLogLeg(ctx context.Context, recorder *logevent.Recorder, leg logevent.Leg, phase logevent.Phase, input wsLogLegInput) {
	if recorder == nil {
		return
	}
	status := input.Status
	if status == "" {
		status = logevent.StatusOK
	}
	facet := Facet{
		Concern:             "providers.mitm.wire",
		Transport:           "websocket",
		Direction:           input.Direction,
		Sequence:            input.Sequence,
		CloseReason:         input.CloseReason,
		RequestContentType:  input.RequestContentType,
		ResponseContentType: input.ResponseContentType,
	}
	var event logevent.Event
	event.Path.Leg = leg
	event.Path.Phase = phase
	event.Outcome.Status = status
	event.Outcome.StatusCode = input.StatusCode
	event.Outcome.ErrorCode = input.ErrorCode
	event.Outcome.ErrorMessage = input.ErrorMessage
	event.Outcome.BytesIn = input.BytesIn
	event.Outcome.BytesOut = input.BytesOut
	event.Facets.Set(facet)
	if input.ClientFacet != nil {
		event.Facets.Set(input.ClientFacet)
	}
	recorder.Emit(ctx, event)
}

func (p *Proxy) recordWSFailure(ctx context.Context, recorder *logevent.Recorder, errorCode string, errorMessage string) {
	if recorder == nil {
		return
	}
	recorder.EmitError(ctx, errorCode, errorMessage)
	recorder.Complete(ctx)
}

// recordWSEnd writes the single terminal lifecycle leg for a bridged websocket
// session: the close status, the error/close reason, and the bridged message
// count. It carries no payload and no content summary. The full client and
// upstream frame streams live only in the SQLite capture store; the wire JSONL
// states nothing about their content.
func (p *Proxy) recordWSEnd(ctx context.Context, recorder *logevent.Recorder, captureDir string, messageCount int, closeErr error) {
	closeReason := ""
	status := logevent.StatusOK
	errorMessage := ""
	if closeErr != nil {
		closeReason = closeErr.Error()
		status = logevent.StatusError
		errorMessage = closeReason
	}
	input := newWSLogLegInput(captureDir, "", nil)
	input.Status = status
	input.ErrorMessage = errorMessage
	input.Sequence = messageCount
	input.CloseReason = closeReason
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMCaptureIndex, logevent.PhaseCompleted, input)
}
