package mitm

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
// upstream and records every text frame to the JSONL capture
// stream. The schema matches the dump.py mitmproxy addon under
// research/codex/captures/2026-04-27/ so existing captures and new
// captures slot into the same toolchain.
//
// On error at any stage we close both ends and emit a ws_end record
// with the error message in the close field.
func (p *Proxy) handleWebsocket(w http.ResponseWriter, r *http.Request, provider string, upstream string) {
	cfg := p.config()
	upstreamURL := wsUpstreamURL(upstream, r.URL.RequestURI())
	upstreamHeaders := wsUpstreamHeaders(r.Header)
	requestContentType := r.Header.Get("Content-Type")
	recorder := p.beginWSLogRecorder(r, provider, upstreamURL)
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
		p.log.Warn("mitm.ws.upgrade_failed", "err", err)
		p.recordWSFailure(ctx, recorder, "ws_client_upgrade_failed", err.Error())
		return
	}
	defer func() { _ = clientConn.Close() }()

	p.recordWSStart(ctx, recorder, cfg.CaptureDir, r.Header, upstreamRespHeaders)

	state := &wsRelayState{
		mu:           sync.Mutex{},
		messageCount: 0,
		closeOnce:    sync.Once{},
		closeErr:     nil,
		closeChan:    make(chan struct{}),
	}
	closeBoth := wsCloseBoth(state, clientConn, upstreamConn)
	relay := p.wsMakeRelay(ctx, wsRelayParams{
		state:      state,
		closeBoth:  closeBoth,
		recorder:   recorder,
		captureDir: cfg.CaptureDir,
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
	if recorder != nil {
		recorder.Complete(ctx)
	}
	queueBaselineRefresh(ctx, cfg, provider, p.log)
	p.log.Info("mitm.ws.closed", "url", upstreamURL, "messages", state.messageCount)
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
	state      *wsRelayState
	closeBoth  func(error)
	recorder   *logevent.Recorder
	captureDir string
}

// wsMakeRelay returns the per-direction relay loop. It reads frames
// from src, mirrors them to dst, and records each frame to the JSONL
// capture stream. On any read, write, or capture failure it triggers
// the shared closeBoth so the partner direction also exits.
func (p *Proxy) wsMakeRelay(ctx context.Context, params wsRelayParams) func(src, dst *websocket.Conn, fromClient bool) {
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
			p.recordWSMessage(ctx, wsMessageCaptureInput{
				Recorder:   params.recorder,
				CaptureDir: params.captureDir,
				Payload:    payload,
				Text:       text,
				FromClient: fromClient,
				Sequence:   count,
			})
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
func (p *Proxy) dialWSUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, headers http.Header) (*websocket.Conn, http.Header, int, error) {
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
		return nil, nil, status, fmt.Errorf("dial websocket upstream: %w", err)
	}
	headersCopy := upstreamResp.Header.Clone()
	if upstreamResp.Body != nil {
		_ = upstreamResp.Body.Close()
	}
	return upstreamConn, headersCopy, http.StatusSwitchingProtocols, nil
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
	Payload             *logevent.PayloadView
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
		Payload:             nil,
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
		CapturePath:         filepath.Join(expandHome(input.CaptureDir), "capture.jsonl"),
		RawRequestPath:      "",
		RawResponsePath:     "",
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
	event.Payload = input.Payload
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

// recordWSStart writes the ws_start capture event through the unified sink model.
func (p *Proxy) recordWSStart(ctx context.Context, recorder *logevent.Recorder, captureDir string, requestHeaders http.Header, responseHeaders http.Header) {
	input := newWSLogLegInput(captureDir, requestHeaders.Get("Content-Type"), nil)
	input.ResponseContentType = responseHeaders.Get("Content-Type")
	input.StatusCode = http.StatusSwitchingProtocols
	p.emitWSLogLeg(ctx, recorder, logevent.LegMITMCaptureIndex, logevent.PhaseStarted, input)
}

type wsMessageCaptureInput struct {
	Recorder   *logevent.Recorder
	CaptureDir string
	Payload    []byte
	Text       string
	FromClient bool
	Sequence   int
}

// recordWSMessage writes a single ws_msg capture event through the unified sink model.
func (p *Proxy) recordWSMessage(ctx context.Context, input wsMessageCaptureInput) {
	direction := "upstream_to_client"
	bytesIn := int64(0)
	bytesOut := int64(len(input.Payload))
	if input.FromClient {
		direction = "client_to_upstream"
		bytesIn = int64(len(input.Payload))
		bytesOut = 0
	}
	payload := logevent.FilterPayload([]byte(input.Text), "text/plain")
	legInput := newWSLogLegInput(input.CaptureDir, "", nil)
	legInput.Direction = direction
	legInput.Sequence = input.Sequence
	legInput.BytesIn = bytesIn
	legInput.BytesOut = bytesOut
	legInput.Payload = &payload
	p.emitWSLogLeg(ctx, input.Recorder, logevent.LegMITMCaptureIndex, logevent.PhaseCompleted, legInput)
}

// recordWSEnd writes the terminal ws_end capture event through the unified sink model.
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
