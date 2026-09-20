// Package reorientinject rewrites a compaction request so the newest
// conversation content stays out of the summary, then inserts that content into
// the summary the model returns. The client persists both in its compact
// summary message and sends them on later turns.
package reorientinject

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/exportargs"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/util"
)

const (
	messagesPathSuffix     = "/v1/messages"
	eventStreamContentType = "text/event-stream"

	reorientInjectConcern   = "providers.mitm.wire"
	reorientInjectComponent = "mitm"
)

// DefaultMaxTokens is the budget a compaction retains when the operator typed
// no max-tokens argument and the configuration sets none.
const DefaultMaxTokens = 500_000

// Settings configures one hook.
type Settings struct {
	// DefaultBudget is the token budget for a compaction whose arguments name
	// no max-tokens. Zero uses DefaultMaxTokens.
	DefaultBudget int
	// Counter measures the retained content. The daemon builds the counter
	// clyde conversation export uses, under the same [export] settings.
	Counter tokencount.Counter
}

// Hook rewrites a compaction request and its summary response. It implements
// [mitm.RequestResponseHook].
type Hook struct {
	provider Provider
	settings Settings
}

// New constructs a compaction split hook. A nil provider or a nil counter
// yields a hook that leaves every request alone.
func New(provider Provider, settings Settings) *Hook {
	if settings.DefaultBudget <= 0 {
		settings.DefaultBudget = DefaultMaxTokens
	}
	return &Hook{provider: provider, settings: settings}
}

// MatchRequestResponse decides whether this request is a compaction, plans the
// cut, and pairs the request rewrite with the response injection. Every failure
// path forwards the request unmodified and leaves the response unchanged.
func (h *Hook) MatchRequestResponse(
	req mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if h.provider == nil || h.settings.Counter == nil {
		return unmatched(), nil
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.Path, messagesPathSuffix) {
		return unmatched(), nil
	}
	// Claude Code declares its own compaction turn on the request. A pasted
	// transcript reproduces the compaction prompt but sets no declaration, so
	// this check precedes the body read and the decode.
	if req.Purpose != mitm.RequestPurposeCompaction {
		return unmatched(), nil
	}
	body, ok := readRequestBody(req.Body)
	if !ok {
		return unmatched(), nil
	}
	parsed, ok := h.provider.ParseCompaction(body)
	if !ok {
		return unmatched(), nil
	}

	budget, include := h.options(parsed.Arguments)
	plan, planned := planCut(parsed, budget, h.settings.Counter, include)
	if !planned {
		logFallback(fallbackNoCut, len(parsed.Messages))
		return unmatched(), nil
	}
	slog.Info("mitm.reorient_inject.split_planned",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
		"message_count", len(parsed.Messages),
		"message_index", plan.Cut.MessageIndex,
		"segment_index", plan.Cut.SegmentIndex,
		"head_runes", plan.Cut.HeadRunes,
		"budget", budget,
		"retained_tokens", plan.RetainedTokens,
	)

	state := &splitState{content: plan.Retained}
	return mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: summaryInjector{
			provider: h.provider,
			state:    state,
		},
		RequestTransformer: requestTruncator{
			provider:         h.provider,
			cut:              plan.Cut,
			instructionStart: parsed.InstructionStart,
			state:            state,
		},
		ContinueMatching: false,
	}, nil
}

// readRequestBody returns the request body, or ok=false when the proxy cannot
// produce it. The hook reports no match in that case. Returning the read error
// instead would surface through the seam and fail the request.
func readRequestBody(source mitm.RequestResponseHookBody) ([]byte, bool) {
	body, err := source.Bytes()
	if err != nil {
		slog.Warn("mitm.reorient_inject.request_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		logFallback(fallbackBodyUnreadable, 0)
		return nil, false
	}
	return body, true
}

// options reads the token budget and the content selection from the arguments
// the operator typed. An unreadable argument never fails a compaction: the
// budget falls back to the configured default and the selection to every kind.
func (h *Hook) options(args []string) (int, func(SegmentKind) bool) {
	budget := h.settings.DefaultBudget
	include := func(SegmentKind) bool { return true }
	if len(args) == 0 {
		return budget, include
	}
	options, err := exportargs.Parse(args)
	if err != nil {
		slog.Warn("mitm.reorient_inject.arguments_invalid",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return budget, include
	}
	if options.MaxTokens != "" {
		parsed, parseErr := util.ParseHumanCount(options.MaxTokens)
		if parseErr != nil {
			slog.Warn("mitm.reorient_inject.max_tokens_invalid",
				"component", reorientInjectComponent,
				"concern", reorientInjectConcern,
				"max_tokens", options.MaxTokens,
				"err", parseErr,
			)
		} else if parsed > 0 {
			budget = parsed
		}
	}
	return budget, includeFor(options.Content)
}

// includeFor maps the selected content kinds onto the segment kinds the split
// counts and retains.
func includeFor(selected conversation.ContentKindSet) func(SegmentKind) bool {
	return func(kind SegmentKind) bool {
		switch kind {
		case KindText:
			return selected.Has(conversation.ContentKindChat)
		case KindThinking:
			return selected.Has(conversation.ContentKindThinking)
		case KindToolUse:
			return selected.Has(conversation.ContentKindToolCalls) ||
				selected.Has(conversation.ContentKindToolSummaries)
		case KindToolResult:
			return selected.Has(conversation.ContentKindToolOutputs)
		case KindImage, KindOther:
			return false
		}
		return false
	}
}

// fallbackReason names why a compaction kept its whole conversation.
type fallbackReason string

const (
	fallbackBodyUnreadable fallbackReason = "body_unreadable"
	fallbackNoCut          fallbackReason = "no_cut"
	fallbackTruncateFailed fallbackReason = "truncate_failed"
)

func logFallback(reason fallbackReason, messageCount int) {
	slog.Warn("mitm.reorient_inject.split_fallback",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
		"reason", string(reason),
		"message_count", messageCount,
	)
}

// splitState passes the retained content from the request rewrite to the
// response injection of the same exchange. The proxy runs the request transform
// before it reads the response, so no lock is needed. An empty content means
// the request went upstream unmodified.
type splitState struct {
	content string
}

// requestTruncator rewrites the compaction request to end the conversation at
// the cut.
type requestTruncator struct {
	provider         Provider
	cut              Cut
	instructionStart int
	state            *splitState
}

func (t requestTruncator) TransformRequest(
	ctx context.Context,
	body []byte,
) ([]byte, bool, error) {
	truncated, err := t.provider.Truncate(body, t.cut, t.instructionStart)
	if err != nil {
		// The proxy forwards the original body on an error. Clearing the state
		// leaves the response unchanged as well, so the compaction runs the way
		// it would without clyde.
		t.state.content = ""
		slog.WarnContext(ctx, "mitm.reorient_inject.request_truncate_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		logFallback(fallbackTruncateFailed, 0)
		return body, false, nil
	}
	return truncated, true, nil
}

// summaryInjector inserts the retained content into the summary response.
type summaryInjector struct {
	provider Provider
	state    *splitState
}

func (t summaryInjector) TransformResponse(
	ctx context.Context,
	resp mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	if t.state.content == "" || !streamingSuccess(resp) {
		// An upstream error body and a non-stream body both reach the client
		// intact.
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.WarnContext(ctx, "mitm.reorient_inject.response_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	injected, err := t.provider.InjectSummary(body, t.state.content)
	if err != nil {
		slog.WarnContext(ctx, "mitm.reorient_inject.summary_inject_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	slog.InfoContext(ctx, "mitm.reorient_inject.matched",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
	)
	return responseWithBody(resp, injected), nil
}

// streamingSuccess reports whether the response is a 200 SSE stream, the only
// shape the summary injection is valid for.
func streamingSuccess(resp mitm.ResponseHookResponse) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return strings.Contains(
		strings.ToLower(resp.Header.Get("Content-Type")),
		eventStreamContentType,
	)
}

func responseWithBody(resp mitm.ResponseHookResponse, body []byte) mitm.ResponseHookResponse {
	header := resp.Header.Clone()
	header.Del("Content-Length")
	return mitm.ResponseHookResponse{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Proto:         resp.Proto,
		Header:        header,
		Body:          bytes.NewReader(body),
		ContentLength: -1,
	}
}

func unmatched() mitm.RequestResponseHookMatch {
	return mitm.RequestResponseHookMatch{
		Matched:            false,
		Transformer:        nil,
		RequestTransformer: nil,
		ContinueMatching:   false,
	}
}
