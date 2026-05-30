package mitm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
)

// mitmRequestIdentity builds the generic [logevent.Identity] for an
// intercepted request from the typed correlation context plus any
// preferred identity hints contributed by the registered provider
// that claims the request. The generic MITM emit path is
// provider-agnostic; provider-specific identity headers are folded
// into the typed [logevent.Facet] returned alongside.
func mitmRequestIdentity(headers http.Header, contrib IdentityContribution) logevent.Identity {
	corr := clydeingress.FromHTTPHeader(headers, headers.Get(clydeingress.HeaderRequestID))
	var identity logevent.Identity
	identity.TraceID = string(corr.TraceID)
	identity.SpanID = string(corr.SpanID)
	identity.ParentSpanID = string(corr.ParentSpanID)
	identity.RequestID = firstNonEmptyString(corr.RequestID, contrib.PreferredRequestID)
	identity.UpstreamRequestID = firstNonEmptyString(clydeingress.UpstreamRequestID(corr), contrib.PreferredUpstreamRequestID)
	identity.UpstreamResponseID = clydeingress.UpstreamResponseID(corr)
	identity.ChatKey = clydeingress.ChatKey(corr)
	identity.ChatKeySource = clydeingress.ChatKeySource(corr)
	identity.ChatRootKey = clydeingress.ChatRootKey(corr)
	identity.ChatBranchKey = clydeingress.ChatBranchKey(corr)
	identity.SessionID = contrib.SessionID
	return identity
}

// extractIdentityContribution asks the first registered provider
// that classifies the supplied request to produce its typed
// identity contribution. When no provider claims the request the
// helper returns the zero value; the generic MITM emit path then
// attaches no provider facet.
func extractIdentityContribution(host, path string, headers http.Header) IdentityContribution {
	if provider, _, ok := providerForPlain(path); ok {
		if contrib := provider.ExtractIdentity(headers); !identityContributionEmpty(contrib) {
			return contrib
		}
	}
	if provider, _, ok := providerForConnect(host); ok {
		if contrib := provider.ExtractIdentity(headers); !identityContributionEmpty(contrib) {
			return contrib
		}
	}
	for _, provider := range defaultRegistry.snapshot() {
		if contrib := provider.ExtractIdentity(headers); !identityContributionEmpty(contrib) {
			return contrib
		}
	}
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		Facet:                      nil,
	}
}

func identityContributionEmpty(contrib IdentityContribution) bool {
	return contrib.PreferredRequestID == "" &&
		contrib.PreferredUpstreamRequestID == "" &&
		contrib.SessionID == "" &&
		contrib.Facet == nil
}

func (p *Proxy) beginMITMLogRecorder(headers http.Header, path logevent.Path) *logevent.Recorder {
	if p == nil || p.requestLog == nil {
		return nil
	}
	contrib := extractIdentityContribution(path.Host, path.Path, headers)
	return p.requestLog.Begin(mitmRequestIdentity(headers, contrib), path)
}

func (p *Proxy) beginHTTPLogRecorder(r *http.Request, input *httpCaptureRecordInput) *logevent.Recorder {
	var path logevent.Path
	path.Surface = logevent.SurfaceMITMIDE
	path.RouteFamily = logevent.RouteFamilyMITMProxy
	path.Path = r.URL.Path
	path.Method = r.Method
	path.Host = r.Host
	path.Provider = input.provider
	path.UpstreamURL = input.upstreamURL
	contrib := extractIdentityContribution(r.Host, r.URL.Path, r.Header)
	if input.clientFacet == nil {
		input.clientFacet = contrib.Facet
	}
	if p == nil || p.requestLog == nil {
		return nil
	}
	contrib.Facet = input.clientFacet
	return p.requestLog.Begin(mitmRequestIdentity(r.Header, contrib), path)
}

func (p *Proxy) emitHTTPLogLeg(ctx context.Context, recorder *logevent.Recorder, leg logevent.Leg, phase logevent.Phase, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	mitmFacet := Facet{
		Concern:             "providers.mitm.wire",
		Transport:           "http",
		Direction:           "",
		Sequence:            0,
		CloseReason:         "",
		RequestContentType:  "",
		ResponseContentType: "",
		// CapturePath is intentionally empty: the dedicated capture.jsonl sink
		// is gone, so each MITM leg lands in the per-concern wire log via the
		// slog router rather than a record-embedded capture-file path.
		CapturePath:     "",
		RawRequestPath:  rawPathFromCaptureBodyIndex(input.requestIndex),
		RawResponsePath: rawPathFromCaptureBodyIndex(input.responseIndex),
	}
	var event logevent.Event
	event.Path.Leg = leg
	event.Path.Phase = phase
	event.Path.Provider = input.provider
	event.Path.UpstreamURL = input.upstreamURL
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.StatusCode = input.responseStatus
	event.Outcome.Duration = input.duration
	event.Outcome.BytesIn = int64(len(input.requestBody))
	event.Outcome.BytesOut = input.responseLen
	event.Facets.Set(mitmFacet)
	if input.clientFacet != nil {
		event.Facets.Set(input.clientFacet)
	}
	recorder.Emit(ctx, event)
}

func (p *Proxy) emitHTTPPayloadLeg(ctx context.Context, recorder *logevent.Recorder, r *http.Request, responseHeader http.Header, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	payload := logevent.FilterPayload(input.requestBody, r.Header.Get("Content-Type"))
	mitmFacet := Facet{
		Concern:             "providers.mitm.wire",
		Transport:           "http",
		Direction:           "",
		Sequence:            0,
		CloseReason:         "",
		RequestContentType:  r.Header.Get("Content-Type"),
		ResponseContentType: responseHeader.Get("Content-Type"),
		// CapturePath is intentionally empty: the dedicated capture.jsonl sink
		// is gone, so each MITM leg lands in the per-concern wire log via the
		// slog router rather than a record-embedded capture-file path.
		CapturePath:     "",
		RawRequestPath:  rawPathFromCaptureBodyIndex(input.requestIndex),
		RawResponsePath: rawPathFromCaptureBodyIndex(input.responseIndex),
	}
	var event logevent.Event
	event.Path.Leg = logevent.LegMITMPayload
	event.Path.Phase = logevent.PhaseCompleted
	event.Path.Provider = input.provider
	event.Path.UpstreamURL = input.upstreamURL
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.StatusCode = input.responseStatus
	event.Outcome.Duration = input.duration
	event.Outcome.BytesIn = int64(len(input.requestBody))
	event.Outcome.BytesOut = input.responseLen
	event.Payload = &payload
	event.Facets.Set(mitmFacet)
	if input.clientFacet != nil {
		event.Facets.Set(input.clientFacet)
	}
	recorder.Emit(ctx, event)
}

type httpFailureRecord struct {
	includePayload      bool
	includeUpstreamSend bool
	errorCode           string
	errorMessage        string
}

func buildHTTPFailureCaptureInput(
	cfg config.MITMConfig,
	provider string,
	upstreamURL string,
	requestBody []byte,
	requestIndex captureBodyIndex,
	responseIndex captureBodyIndex,
	duration time.Duration,
	responseStatus int,
) httpCaptureRecordInput {
	return httpCaptureRecordInput{
		config:         cfg,
		provider:       provider,
		upstreamURL:    upstreamURL,
		requestBody:    requestBody,
		responseBody:   nil,
		requestIndex:   requestIndex,
		responseIndex:  responseIndex,
		responseLen:    0,
		duration:       duration,
		responseStatus: responseStatus,
		clientFacet:    nil,
	}
}

func (p *Proxy) recordHTTPFailure(r *http.Request, responseHeader http.Header, input httpCaptureRecordInput, failure httpFailureRecord) {
	if p == nil || r == nil || strings.TrimSpace(input.provider) == "" {
		return
	}
	recorder := p.beginHTTPLogRecorder(r, &input)
	if recorder == nil {
		return
	}
	ctx := r.Context()
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMIngress, logevent.PhaseStarted, input)
	if failure.includePayload {
		p.emitHTTPPayloadLeg(ctx, recorder, r, responseHeader, input)
	}
	if failure.includeUpstreamSend {
		p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMUpstreamSend, logevent.PhaseStarted, input)
	}
	recorder.EmitError(ctx, failure.errorCode, failure.errorMessage)
	recorder.Complete(ctx)
}

func rawPathFromCaptureBodyIndex(index captureBodyIndex) string {
	if len(index.raw) == 0 {
		return ""
	}
	var reference struct {
		RawPath string `json:"raw_path"`
	}
	if err := json.Unmarshal(index.raw, &reference); err != nil {
		return ""
	}
	return reference.RawPath
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (p *Proxy) completeHTTPLogRecorder(ctx context.Context, recorder *logevent.Recorder, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMComplete, logevent.PhaseCompleted, input)
	recorder.Complete(ctx)
}
