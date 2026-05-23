package mitm

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"

	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/logevent"
)

func (p *Proxy) beginHTTPLogRecorder(r *http.Request, input httpCaptureRecordInput) *logevent.Recorder {
	if p == nil || p.requestLog == nil {
		return nil
	}
	corr := correlation.FromHTTPHeader(r.Header, r.Header.Get(correlation.HeaderRequestID))
	var identity logevent.Identity
	identity.TraceID = string(corr.TraceID)
	identity.SpanID = string(corr.SpanID)
	identity.ParentSpanID = string(corr.ParentSpanID)
	identity.RequestID = corr.RequestID
	identity.CursorRequestID = corr.CursorRequestID
	identity.CursorConversationID = corr.CursorConversationID
	identity.CursorGenerationID = corr.CursorGenerationID
	identity.UpstreamRequestID = corr.UpstreamRequestID
	identity.UpstreamResponseID = corr.UpstreamResponseID
	identity.ChatKey = corr.ChatKey
	identity.ChatKeySource = corr.ChatKeySource
	identity.ChatRootKey = corr.ChatRootKey
	identity.ChatBranchKey = corr.ChatBranchKey
	var path logevent.Path
	path.Surface = logevent.SurfaceMITMIDE
	path.RouteFamily = logevent.RouteFamilyMITMProxy
	path.Path = r.URL.Path
	path.Method = r.Method
	path.Host = r.Host
	path.Provider = input.provider
	path.UpstreamURL = input.upstreamURL
	return p.requestLog.Begin(identity, path)
}

func (p *Proxy) emitHTTPLogLeg(ctx context.Context, recorder *logevent.Recorder, leg logevent.Leg, phase logevent.Phase, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	var facet logevent.MITMFacet
	facet.Concern = "providers.mitm.wire"
	facet.Transport = "http"
	facet.CapturePath = filepath.Join(expandHome(input.config.CaptureDir), "capture.jsonl")
	facet.RawRequestPath = rawPathFromCaptureBodyIndex(input.requestIndex)
	facet.RawResponsePath = rawPathFromCaptureBodyIndex(input.responseIndex)
	var providerFacets logevent.ProviderFacets
	providerFacets.MITM = &facet
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
	event.Facets = providerFacets
	recorder.Emit(ctx, event)
}

func (p *Proxy) emitHTTPPayloadLeg(ctx context.Context, recorder *logevent.Recorder, r *http.Request, responseHeader http.Header, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	payload := logevent.FilterPayload(input.requestBody, r.Header.Get("Content-Type"))
	var facet logevent.MITMFacet
	facet.Concern = "providers.mitm.wire"
	facet.Transport = "http"
	facet.RequestContentType = r.Header.Get("Content-Type")
	facet.ResponseContentType = responseHeader.Get("Content-Type")
	facet.CapturePath = filepath.Join(expandHome(input.config.CaptureDir), "capture.jsonl")
	facet.RawRequestPath = rawPathFromCaptureBodyIndex(input.requestIndex)
	facet.RawResponsePath = rawPathFromCaptureBodyIndex(input.responseIndex)
	var providerFacets logevent.ProviderFacets
	providerFacets.MITM = &facet
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
	event.Facets = providerFacets
	recorder.Emit(ctx, event)
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

func (p *Proxy) completeHTTPLogRecorder(ctx context.Context, recorder *logevent.Recorder, input httpCaptureRecordInput) {
	if recorder == nil {
		return
	}
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMComplete, logevent.PhaseCompleted, input)
	recorder.Complete(ctx)
}
