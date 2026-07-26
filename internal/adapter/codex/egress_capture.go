package codex

import (
	"bytes"
	"net/http"
	"net/url"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

// captureClientCodex is the capture.db `client` tag for adapter Codex BYOK
// egress, distinct from the cli.codex passthrough the wire baseline is learned
// from.
const captureClientCodex = "adapter.codex"

// captureCodexConcern is the capture.db `concern` tag for adapter Codex egress
// rows.
const captureCodexConcern = "adapter.codex.egress"

// codexCaptureBodyCap bounds how many response bytes one egress capture buffers
// in memory before truncating. It matches the Anthropic egress cap so the
// capture store keeps full bodies for all but pathological streams.
const codexCaptureBodyCap = 8 << 20

// codexEgressRecord carries the fields recordCodexEgress writes for one
// outbound Codex exchange. url is the full responses URL (HTTP or wss); host
// and path are derived from it.
type codexEgressRecord struct {
	url               string
	method            string
	reqType           string
	respType          string
	upstreamRequestID string
	sessionID         string
	status            int
	reqHeaders        http.Header
	respHeaders       http.Header
	reqBody           []byte
	respBody          []byte
	started           time.Time
}

// wsEgressCapture carries the values the websocket reader goroutine needs to
// record one full-frame exchange on loop exit. It is nil for warmup turns and
// when no capture store is configured, which disables accumulation and the
// record.
type wsEgressCapture struct {
	store     *capture.Store
	corr      correlation.Context
	url       string
	outbound  []byte
	sessionID string
	started   time.Time
}

// wsFrameRecorder accumulates inbound websocket frames and records the full
// exchange once on loop exit. A nil capture config disables both accumulation
// and the record, so a non-capturing or warmup turn costs nothing.
type wsFrameRecorder struct {
	cfg      *wsEgressCapture
	inbound  [][]byte
	recorded bool
}

func newWsFrameRecorder(cfg *wsEgressCapture) *wsFrameRecorder {
	return &wsFrameRecorder{cfg: cfg, inbound: nil, recorded: false}
}

// add appends one inbound text frame to the accumulated stream.
func (r *wsFrameRecorder) add(message []byte) {
	if r.cfg == nil {
		return
	}
	r.inbound = append(r.inbound, message)
}

// record writes the accumulated exchange exactly once. status is 200 on a
// clean completion and 0 on an I/O error or panic.
func (r *wsFrameRecorder) record(status int) {
	if r.cfg == nil || r.recorded {
		return
	}
	r.recorded = true
	recordCodexEgress(r.cfg.store, r.cfg.corr, codexEgressRecord{
		url:               r.cfg.url,
		method:            "WEBSOCKET",
		reqType:           "application/json",
		respType:          "text/event-stream",
		upstreamRequestID: "",
		sessionID:         r.cfg.sessionID,
		status:            status,
		reqHeaders:        nil,
		respHeaders:       nil,
		reqBody:           r.cfg.outbound,
		respBody:          joinFrames(r.inbound),
		started:           r.cfg.started,
	})
}

// recordCodexEgress writes one outbound Codex exchange to the capture store
// tagged client="adapter.codex". A nil store is a no-op; the store's Record is
// asynchronous and non-blocking.
func recordCodexEgress(store *capture.Store, corr correlation.Context, in codexEgressRecord) {
	if store == nil {
		return
	}
	host, path := "", ""
	if parsed, err := url.Parse(in.url); err == nil {
		host = parsed.Host
		path = parsed.Path
	}
	store.RecordExchange(corr, capture.Exchange{
		Client:            captureClientCodex,
		Provider:          "codex",
		Concern:           captureCodexConcern,
		Host:              host,
		Method:            in.method,
		Path:              path,
		Status:            in.status,
		UpstreamRequestID: in.upstreamRequestID,
		SessionID:         in.sessionID,
		RequestHeaders:    in.reqHeaders,
		ResponseHeaders:   in.respHeaders,
		RequestBody:       in.reqBody,
		ResponseBody:      in.respBody,
		RequestType:       in.reqType,
		ResponseType:      in.respType,
		Conversation:      capture.UnresolvedConversation(),
		Started:           in.started,
	})
}

// recordCodexHTTPEgress records one Codex HTTP Responses exchange to the
// capture store from the outbound request, the upstream response, and the
// (capped) streamed response body.
func recordCodexHTTPEgress(store *capture.Store, corr correlation.Context, req *http.Request, resp *http.Response, reqBody, respBody []byte, conversationID string, started time.Time) {
	recordCodexEgress(store, corr, codexEgressRecord{
		url:               req.URL.String(),
		method:            req.Method,
		reqType:           "application/json",
		respType:          resp.Header.Get("Content-Type"),
		upstreamRequestID: resp.Header.Get("X-Request-Id"),
		sessionID:         conversationID,
		status:            resp.StatusCode,
		reqHeaders:        req.Header,
		respHeaders:       resp.Header,
		reqBody:           reqBody,
		respBody:          respBody,
		started:           started,
	})
}

// joinFrames concatenates websocket frames into one newline-delimited byte
// slice so the full inbound or outbound frame stream lands in a single capture
// body, capped at codexCaptureBodyCap.
func joinFrames(frames [][]byte) []byte {
	var out bytes.Buffer
	for i, frame := range frames {
		if i > 0 {
			out.WriteByte('\n')
		}
		if out.Len() >= codexCaptureBodyCap {
			break
		}
		out.Write(frame)
	}
	return out.Bytes()
}
