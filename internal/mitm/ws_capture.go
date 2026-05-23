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
	"goodkind.io/clyde/internal/correlation"
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

	p.recordWSStart(r.Context(), cfg.CaptureDir, provider, upstreamURL, r.Header, upstreamRespHeaders, corr)

	state := &wsRelayState{
		mu:           sync.Mutex{},
		messageCount: 0,
		closeOnce:    sync.Once{},
		closeErr:     nil,
		closeChan:    make(chan struct{}),
	}
	closeBoth := wsCloseBoth(state, clientConn, upstreamConn)
	relay := p.wsMakeRelay(r.Context(), wsRelayParams{
		state:       state,
		closeBoth:   closeBoth,
		provider:    provider,
		upstreamURL: upstreamURL,
		captureDir:  cfg.CaptureDir,
		corr:        corr,
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

	p.recordWSEnd(r.Context(), cfg.CaptureDir, provider, upstreamURL, state.messageCount, corr, state.closeErr)
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
	state       *wsRelayState
	closeBoth   func(error)
	provider    string
	upstreamURL string
	captureDir  string
	corr        correlation.Context
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
				CaptureDir:  params.captureDir,
				Provider:    params.provider,
				UpstreamURL: params.upstreamURL,
				Payload:     payload,
				Text:        text,
				FromClient:  fromClient,
				Sequence:    count,
				Correlation: params.corr,
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

// recordWSStart writes the ws_start capture event through the unified sink model.
func (p *Proxy) recordWSStart(ctx context.Context, captureDir string, provider string, upstreamURL string, requestHeaders http.Header, responseHeaders http.Header, corr correlation.Context) {
	requestContentType := requestHeaders.Get("Content-Type")
	responseContentType := responseHeaders.Get("Content-Type")
	var input wsCaptureEventInput
	input.CaptureDir = captureDir
	input.Provider = provider
	input.UpstreamURL = upstreamURL
	input.Correlation = corr
	input.Transport = "websocket"
	input.Phase = logevent.PhaseStarted
	input.RequestContentType = requestContentType
	input.ResponseContentType = responseContentType
	p.emitWSCaptureEvent(ctx, input)
}

type wsMessageCaptureInput struct {
	CaptureDir  string
	Provider    string
	UpstreamURL string
	Payload     []byte
	Text        string
	FromClient  bool
	Sequence    int
	Correlation correlation.Context
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
	var eventInput wsCaptureEventInput
	eventInput.CaptureDir = input.CaptureDir
	eventInput.Provider = input.Provider
	eventInput.UpstreamURL = input.UpstreamURL
	eventInput.Correlation = input.Correlation
	eventInput.Transport = "websocket"
	eventInput.Direction = direction
	eventInput.Sequence = input.Sequence
	eventInput.Phase = logevent.PhaseCompleted
	eventInput.BytesIn = bytesIn
	eventInput.BytesOut = bytesOut
	eventInput.Payload = &payload
	p.emitWSCaptureEvent(ctx, eventInput)
}

// recordWSEnd writes the terminal ws_end capture event through the unified sink model.
func (p *Proxy) recordWSEnd(ctx context.Context, captureDir string, provider string, upstreamURL string, messageCount int, corr correlation.Context, closeErr error) {
	closeReason := ""
	status := logevent.StatusOK
	errorMessage := ""
	if closeErr != nil {
		closeReason = closeErr.Error()
		status = logevent.StatusError
		errorMessage = closeReason
	}
	var input wsCaptureEventInput
	input.CaptureDir = captureDir
	input.Provider = provider
	input.UpstreamURL = upstreamURL
	input.Correlation = corr
	input.Transport = "websocket"
	input.Sequence = messageCount
	input.CloseReason = closeReason
	input.Phase = logevent.PhaseCompleted
	input.Status = status
	input.Error = errorMessage
	p.emitWSCaptureEvent(ctx, input)
}

type wsCaptureEventInput struct {
	CaptureDir          string
	Provider            string
	UpstreamURL         string
	Correlation         correlation.Context
	Transport           string
	Direction           string
	Sequence            int
	CloseReason         string
	RequestContentType  string
	ResponseContentType string
	Phase               logevent.Phase
	Status              logevent.Status
	Error               string
	BytesIn             int64
	BytesOut            int64
	Payload             *logevent.PayloadView
}

func (p *Proxy) emitWSCaptureEvent(ctx context.Context, input wsCaptureEventInput) {
	if p == nil || p.requestLog == nil {
		return
	}
	var identity logevent.Identity
	identity.TraceID = string(input.Correlation.TraceID)
	identity.SpanID = string(input.Correlation.SpanID)
	identity.ParentSpanID = string(input.Correlation.ParentSpanID)
	identity.RequestID = input.Correlation.RequestID
	identity.CursorRequestID = input.Correlation.CursorRequestID
	identity.CursorConversationID = input.Correlation.CursorConversationID
	identity.CursorGenerationID = input.Correlation.CursorGenerationID
	identity.UpstreamRequestID = input.Correlation.UpstreamRequestID
	identity.UpstreamResponseID = input.Correlation.UpstreamResponseID
	parsedURL, err := url.Parse(input.UpstreamURL)
	pathValue := input.UpstreamURL
	host := ""
	if err == nil {
		pathValue = parsedURL.Path
		host = parsedURL.Host
	}
	status := input.Status
	if status == "" {
		status = logevent.StatusOK
	}
	var outcome logevent.Outcome
	outcome.Status = status
	outcome.ErrorMessage = input.Error
	outcome.BytesIn = input.BytesIn
	outcome.BytesOut = input.BytesOut
	var path logevent.Path
	path.Surface = logevent.SurfaceMITMIDE
	path.RouteFamily = logevent.RouteFamilyMITMProxy
	path.Leg = logevent.LegMITMCaptureIndex
	path.Phase = input.Phase
	path.Path = pathValue
	path.Method = http.MethodGet
	path.Host = host
	path.Provider = input.Provider
	path.UpstreamURL = input.UpstreamURL
	var facet logevent.MITMFacet
	facet.Concern = "providers.mitm.wire"
	facet.Transport = input.Transport
	facet.Direction = input.Direction
	facet.Sequence = input.Sequence
	facet.CloseReason = input.CloseReason
	facet.RequestContentType = input.RequestContentType
	facet.ResponseContentType = input.ResponseContentType
	facet.CapturePath = filepath.Join(expandHome(input.CaptureDir), "capture.jsonl")
	var providerFacets logevent.ProviderFacets
	providerFacets.MITM = &facet
	var event logevent.Event
	event.Identity = identity
	event.Path = path
	event.Outcome = outcome
	event.Payload = input.Payload
	event.Facets = providerFacets
	p.requestLog.Emit(ctx, event)
}
