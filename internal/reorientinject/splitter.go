package reorientinject

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/exportargs"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/util"
)

const (
	reorientInjectConcern   = "providers.mitm.wire"
	reorientInjectComponent = "mitm"
)

// DefaultMaxTokens is the budget a compaction retains when the operator typed
// no max-tokens argument and the configuration sets none.
const DefaultMaxTokens = 500_000

// Settings configures one splitter.
type Settings struct {
	// DefaultBudget is the token budget when the compaction arguments set no
	// max-tokens. Zero uses DefaultMaxTokens.
	DefaultBudget int
	// DefaultContent is the content selection when the compaction arguments
	// pick no content kind. The empty set keeps every kind.
	DefaultContent conversation.ContentKindSet
	// MaxRetainedBytes caps the injection in bytes. Zero means no cap.
	MaxRetainedBytes int
	// InstructionsFile is a markdown file the split appends after the
	// transcript on every compaction. Empty appends nothing. An unreadable
	// file logs a warning and appends nothing.
	InstructionsFile string
	// Counter measures the retained content. The daemon builds the counter
	// clyde conversation export uses, under the same [export] settings.
	Counter tokencount.Counter
}

// Splitter plans one compaction split for any transport. It calls the
// provider to decode the request, plans the cut by token budget, calls the
// provider to truncate the request, and builds the injection text.
type Splitter struct {
	provider Provider
	settings Settings
}

// Result is one planned split.
type Result struct {
	// Forwarded is the truncated request.
	Forwarded []byte
	// Injection is the wrapped text the provider inserts into the summary.
	Injection string
}

// NewSplitter constructs a splitter. A nil provider or a nil counter yields a
// splitter that plans nothing.
func NewSplitter(provider Provider, settings Settings) *Splitter {
	if settings.DefaultBudget <= 0 {
		settings.DefaultBudget = DefaultMaxTokens
	}
	return &Splitter{provider: provider, settings: settings}
}

// Plan decodes body, plans the cut, and truncates. ok is false when body is
// not a compaction request, nothing fits the budget, or the truncation
// fails; the original request is forwarded unmodified in every such case.
func (s *Splitter) Plan(body []byte) (Result, bool) {
	noResult := Result{Forwarded: nil, Injection: ""}
	if s == nil || s.provider == nil || s.settings.Counter == nil {
		return noResult, false
	}
	parsed, ok := s.provider.ParseCompaction(body)
	if !ok {
		return noResult, false
	}
	budget, include := s.options(parsed.Arguments)
	instructions := s.instructions()
	maxBytes := s.settings.MaxRetainedBytes
	if maxBytes > 0 {
		maxBytes -= len(reorienttag.WrapInjection("", instructions))
	}
	plan, planned := planCut(parsed, budget, maxBytes, s.settings.Counter, include)
	if !planned {
		logFallback(fallbackNoCut, len(parsed.Messages))
		return noResult, false
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
	forwarded, err := s.provider.Truncate(body, plan.Cut, parsed.InstructionStart)
	if err != nil {
		slog.Warn("mitm.reorient_inject.request_truncate_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		logFallback(fallbackTruncateFailed, len(parsed.Messages))
		return noResult, false
	}
	return Result{
		Forwarded: forwarded,
		Injection: reorienttag.WrapInjection(plan.Retained, instructions),
	}, true
}

// instructions reads the configured instructions file. It returns an empty
// string when no file is configured or the file cannot be read; the read
// failure is logged and the compaction proceeds without the span.
func (s *Splitter) instructions() string {
	path := s.settings.InstructionsFile
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("mitm.reorient_inject.instructions_unreadable",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"path", path,
			"err", err,
		)
		return ""
	}
	return strings.TrimSpace(string(content))
}

// Inject inserts injection into the summary response body through the
// provider.
func (s *Splitter) Inject(body []byte, injection string) ([]byte, error) {
	injected, err := s.provider.InjectSummary(body, injection)
	if err != nil {
		slog.Warn("mitm.reorient_inject.summary_inject_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return nil, fmt.Errorf("inject compaction summary: %w", err)
	}
	return injected, nil
}

// options reads the token budget and the content kinds from the arguments the
// operator typed. Each falls back to its configured default. An unreadable
// argument never fails a compaction: both fall back to the configured defaults.
func (s *Splitter) options(args []string) (int, func(SegmentKind) bool) {
	budget := s.settings.DefaultBudget
	selected := s.settings.DefaultContent
	if len(args) == 0 {
		return budget, includeFor(selected)
	}
	options, err := exportargs.Parse(args)
	if err != nil {
		slog.Warn("mitm.reorient_inject.arguments_invalid",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return budget, includeFor(selected)
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
	if !options.Content.Empty() {
		selected = options.Content
	}
	return budget, includeFor(selected)
}

// includeFor maps the selected content kinds onto the segment kinds the split
// counts and retains. The empty set keeps every kind.
//
// tool_outputs is the highest-detail tool kind. ResolveContentKinds deletes
// tool_calls from a set that includes tool_outputs, and the export renderer
// then draws each call with its result. The split matches that: tool_outputs
// keeps the calls as well as the results.
func includeFor(selected conversation.ContentKindSet) func(SegmentKind) bool {
	if selected.Empty() {
		return func(SegmentKind) bool { return true }
	}
	return func(kind SegmentKind) bool {
		switch kind {
		case KindText, KindOther:
			return selected.Has(conversation.ContentKindChat)
		case KindThinking:
			return selected.Has(conversation.ContentKindThinking)
		case KindToolUse:
			return selected.Has(conversation.ContentKindToolCalls) ||
				selected.Has(conversation.ContentKindToolSummaries) ||
				selected.Has(conversation.ContentKindToolOutputs)
		case KindToolResult:
			return selected.Has(conversation.ContentKindToolOutputs)
		case KindImage:
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
