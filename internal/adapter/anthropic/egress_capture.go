package anthropic

import (
	"context"
	"net/http"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

// captureClientAnthropic is the capture.db `client` tag for adapter
// Anthropic BYOK egress, distinct from the cli.claude-code passthrough the
// wire baseline is learned from.
const captureClientAnthropic = "adapter.anthropic"

// captureEgressConcern is the capture.db `concern` tag for adapter Anthropic
// egress rows.
const captureEgressConcern = "adapter.anthropic.egress"

// captureStoreBodyCap bounds how many response bytes one egress capture buffers
// in memory before truncating. It is larger than the wire-capture cap so the
// capture store keeps full bodies for all but pathological streams.
const captureStoreBodyCap = 8 << 20

// egressExchange carries the request-side values recorded alongside the
// response when an outbound /v1/messages exchange is written to the capture
// store.
type egressExchange struct {
	method     string
	host       string
	path       string
	reqType    string
	reqHeaders http.Header
	reqBody    []byte
	started    time.Time
}

// attachEgressObservers tees the streamed response body into the SQLite capture
// store so the outbound /v1/messages exchange lands in capture.db with its
// request body and its response body up to captureStoreBodyCap. With no capture
// store configured, resp.Body is left untouched. Bodies are never written to a
// log.
func (c *Client) attachEgressObservers(ctx context.Context, resp *http.Response, base responseEvent, ex egressExchange) {
	if c.cfg.CaptureStore == nil {
		return
	}
	respHeaders := resp.Header.Clone()
	// Detach from request cancellation so the on-close emission still fires
	// after the SSE stream completes; correlation values survive WithoutCancel.
	emitCtx := context.WithoutCancel(ctx)
	resp.Body = newCaptureTee(resp.Body, captureStoreBodyCap, func(captured []byte, _ bool, _ int) {
		c.recordEgress(emitCtx, ex, base.Status, respHeaders, captured)
	})
}

// recordEgress writes one outbound /v1/messages exchange to the capture store
// tagged client="adapter.anthropic" so the BYOK egress lands in capture.db with
// full request and response bodies. A nil store is a no-op; the store's Record
// is asynchronous and non-blocking.
func (c *Client) recordEgress(ctx context.Context, ex egressExchange, status int, respHeaders http.Header, respBody []byte) {
	store := c.cfg.CaptureStore
	if store == nil {
		return
	}
	store.RecordExchange(correlation.FromContext(ctx), capture.Exchange{
		Client:            captureClientAnthropic,
		Provider:          "anthropic",
		Concern:           captureEgressConcern,
		Host:              ex.host,
		Method:            ex.method,
		Path:              ex.path,
		Status:            status,
		UpstreamRequestID: respHeaders.Get("Request-Id"),
		SessionID:         sessionID,
		RequestHeaders:    ex.reqHeaders,
		ResponseHeaders:   respHeaders,
		RequestBody:       ex.reqBody,
		ResponseBody:      respBody,
		RequestType:       ex.reqType,
		ResponseType:      respHeaders.Get("Content-Type"),
		Started:           ex.started,
	})
}
