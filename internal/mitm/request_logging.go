package mitm

import (
	"context"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/providerid"
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
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}

func identityContributionEmpty(contrib IdentityContribution) bool {
	return contrib.PreferredRequestID == "" &&
		contrib.PreferredUpstreamRequestID == "" &&
		contrib.SessionID == "" &&
		contrib.ConversationID == "" &&
		contrib.Facet == nil
}

// captureConversationFields derives the Clyde conversation id and
// source tag stored on MITM capture rows from the provider label and
// native conversation id contributed by the provider extractor.
func captureConversationFields(provider string, contrib IdentityContribution) (conversationID string, conversationSource string) {
	nativeID := strings.TrimSpace(contrib.ConversationID)
	if nativeID == "" {
		return "", ""
	}
	prov, ok := providerid.Parse(provider)
	if !ok || prov == providerid.ProviderUnspecified {
		return "", ""
	}
	source := strings.TrimSpace(contrib.ConversationSource)
	if source == "" {
		source = "header"
	}
	return conversation.DerivedID(prov, nativeID, ""), source
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
	mitmFacet := Facet{
		Concern:             "providers.mitm.wire",
		Transport:           "http",
		Direction:           "",
		Sequence:            0,
		CloseReason:         "",
		RequestContentType:  r.Header.Get("Content-Type"),
		ResponseContentType: responseHeader.Get("Content-Type"),
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
