package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

type encodeJSON func(any) ([]byte, error)

type UsageWindowNoticeInput struct {
	Provider    string
	WindowKey   string
	LimitLabel  string
	UsedPercent float64
	ResetsAt    time.Time
	Kind        string
}

type UsageNotice struct {
	Kind       string
	Text       string
	ResetsAt   time.Time
	Threshold  float64
	WindowKey  string
	Provider   string
	LimitLabel string
}

type usageNoticeRecord struct {
	ResetAt            time.Time
	LastEmittedAt      time.Time
	TurnsSinceEmission int
}

// UsageNoticeGate decides which usage windows should surface a user-visible
// notice and at what cadence. The gate owns a structured logger so every
// evaluate decision (suppressed, due, or skipped) can be traced back to the
// exact provider, window_key, and limit_label that produced it.
type UsageNoticeGate struct {
	mu      sync.Mutex
	records map[string]usageNoticeRecord
	log     *slog.Logger
}

// NewUsageNoticeGateWithLogger constructs a gate with an explicit logger.
// The logger is wrapped with the adapter notice concern so every event lands
// in the dedicated notice log.
func NewUsageNoticeGateWithLogger(log *slog.Logger) *UsageNoticeGate {
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernAdapterNotice)
	return &UsageNoticeGate{
		records: make(map[string]usageNoticeRecord),
		log:     log,
	}
}

func (g *UsageNoticeGate) Evaluate(
	windows []UsageWindowNoticeInput,
	notices config.AdapterNotices,
	now time.Time,
) []UsageNotice {
	if g == nil || !notices.EnabledOrDefault() || len(windows) == 0 {
		return nil
	}
	policy := notices.UsageRepeatPolicyOrDefault()
	thresholds := notices.UsageThresholdsUsedPercentOrDefault()
	logger := g.logger()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.records == nil {
		g.records = make(map[string]usageNoticeRecord)
	}

	due := make([]UsageNotice, 0, len(windows))
	for _, window := range windows {
		notice, ok := evaluateUsageWindow(window, thresholds, now)
		if !ok {
			logger.LogAttrs(context.Background(), slog.LevelDebug, "adapter.notice.skipped",
				slog.String("component", "adapter"),
				slog.String("subcomponent", "usage_notice_gate"),
				slog.String("provider", strings.TrimSpace(window.Provider)),
				slog.String("window_key", strings.TrimSpace(window.WindowKey)),
				slog.String("limit_label", strings.TrimSpace(window.LimitLabel)),
				slog.Float64("used_percent", window.UsedPercent),
				slog.String("reason", "below_threshold_or_invalid_input"),
			)
			continue
		}
		key := usageNoticeRepeatKey(window.Provider, notice.WindowKey, notice.Threshold)
		record := g.records[key]
		if usageNoticeDue(policy, record, notice, now) {
			record.ResetAt = notice.ResetsAt.UTC()
			record.LastEmittedAt = now
			record.TurnsSinceEmission = 0
			g.records[key] = record
			due = append(due, notice)
			logger.LogAttrs(context.Background(), slog.LevelDebug, "adapter.notice.evaluated",
				slog.String("component", "adapter"),
				slog.String("subcomponent", "usage_notice_gate"),
				slog.String("provider", notice.Provider),
				slog.String("window_key", notice.WindowKey),
				slog.String("limit_label", notice.LimitLabel),
				slog.String("kind", notice.Kind),
				slog.Float64("threshold", notice.Threshold),
				slog.Float64("used_percent", window.UsedPercent),
				slog.Time("resets_at", notice.ResetsAt),
				slog.String("decision", "due"),
				slog.String("repeat_mode", string(policy.Mode)),
			)
			continue
		}
		record.ResetAt = notice.ResetsAt.UTC()
		record.TurnsSinceEmission++
		g.records[key] = record
		logger.LogAttrs(context.Background(), slog.LevelDebug, "adapter.notice.suppressed",
			slog.String("component", "adapter"),
			slog.String("subcomponent", "usage_notice_gate"),
			slog.String("provider", notice.Provider),
			slog.String("window_key", notice.WindowKey),
			slog.String("limit_label", notice.LimitLabel),
			slog.String("kind", notice.Kind),
			slog.Float64("threshold", notice.Threshold),
			slog.Float64("used_percent", window.UsedPercent),
			slog.Time("resets_at", notice.ResetsAt),
			slog.String("decision", "suppressed"),
			slog.String("repeat_mode", string(policy.Mode)),
			slog.Int("turns_since_emission", record.TurnsSinceEmission),
		)
	}
	if len(due) > 0 {
		providers := make([]string, 0, len(due))
		windowKeys := make([]string, 0, len(due))
		limitLabels := make([]string, 0, len(due))
		for _, notice := range due {
			providers = append(providers, notice.Provider)
			windowKeys = append(windowKeys, notice.WindowKey)
			limitLabels = append(limitLabels, notice.LimitLabel)
		}
		logger.LogAttrs(context.Background(), slog.LevelInfo, "adapter.notice.evaluate_summary",
			slog.String("component", "adapter"),
			slog.String("subcomponent", "usage_notice_gate"),
			slog.Int("due_count", len(due)),
			slog.Int("window_count", len(windows)),
			slog.Any("providers", providers),
			slog.Any("window_keys", windowKeys),
			slog.Any("limit_labels", limitLabels),
			slog.String("repeat_mode", string(policy.Mode)),
		)
	}
	return due
}

func (g *UsageNoticeGate) logger() *slog.Logger {
	if g == nil || g.log == nil {
		return slogger.WithConcern(slog.Default(), slogger.ConcernAdapterNotice)
	}
	return g.log
}

func evaluateUsageWindow(
	window UsageWindowNoticeInput,
	thresholdsUsedPercent []float64,
	now time.Time,
) (UsageNotice, bool) {
	provider := strings.TrimSpace(window.Provider)
	windowKey := strings.TrimSpace(window.WindowKey)
	limitLabel := strings.TrimSpace(window.LimitLabel)
	if provider == "" || windowKey == "" || limitLabel == "" {
		return UsageNotice{}, false
	}
	highestThreshold, ok := highestUsageThreshold(window.UsedPercent, thresholdsUsedPercent)
	if !ok {
		return UsageNotice{}, false
	}
	kind := strings.TrimSpace(window.Kind)
	if kind == "" {
		kind = fmt.Sprintf(
			"%s_%s_threshold_%s",
			provider,
			windowKey,
			formatThresholdForKind(highestThreshold),
		)
	}
	return UsageNotice{
		Kind:       kind,
		Text:       usageNoticeText(provider, usageRemainingPercent(window.UsedPercent), limitLabel, window.ResetsAt, now),
		ResetsAt:   window.ResetsAt.UTC(),
		Threshold:  highestThreshold,
		WindowKey:  windowKey,
		Provider:   provider,
		LimitLabel: limitLabel,
	}, true
}

func highestUsageThreshold(usedPercent float64, thresholdsUsedPercent []float64) (float64, bool) {
	var highest float64
	matched := false
	for _, threshold := range thresholdsUsedPercent {
		if usedPercent >= threshold {
			highest = threshold
			matched = true
		}
	}
	return highest, matched
}

func usageRemainingPercent(usedPercent float64) int {
	remaining := 100 - usedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	return int(remaining + 0.5)
}

func formatThresholdForKind(threshold float64) string {
	formatted := fmt.Sprintf("%.2f", threshold)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimRight(formatted, ".")
	return strings.ReplaceAll(formatted, ".", "_")
}

// usageNoticeText renders the user-visible warning. Provider is included
// inline so users can tell at a glance which upstream provider is constrained
// (e.g. "Codex" vs "Anthropic"); when provider is missing the prefix is
// dropped to avoid an empty label colon.
func usageNoticeText(provider string, remainingPercent int, limitLabel string, resetsAt time.Time, now time.Time) string {
	prefix := "⚠️"
	if label := providerDisplayLabel(provider); label != "" {
		prefix = "⚠️ " + label + ":"
	}
	return fmt.Sprintf(
		"%s You have about %d%% of your %s limit left. The limit resets %s.",
		prefix,
		remainingPercent,
		limitLabel,
		usageResetPhrase(resetsAt, now),
	)
}

// providerDisplayLabel converts a provider id (lowercase, snake/dotted) into
// a human-friendly label that fits the user-visible notice text.
func providerDisplayLabel(provider string) string {
	trimmed := strings.TrimSpace(provider)
	switch strings.ToLower(trimmed) {
	case "":
		return ""
	case "codex":
		return "Codex"
	case "anthropic":
		return "Anthropic"
	default:
		lower := strings.ToLower(trimmed)
		return strings.ToUpper(lower[:1]) + lower[1:]
	}
}

func usageResetPhrase(resetsAt time.Time, now time.Time) string {
	if resetsAt.IsZero() {
		return "at an unknown time"
	}
	localReset := resetsAt.In(now.Location())
	zone, _ := localReset.Zone()
	if zone == "" {
		zone = localReset.Format("MST")
	}
	return fmt.Sprintf(
		"in %s on %s at %s %s",
		formatResetDuration(localReset.Sub(now.In(localReset.Location()))),
		localReset.Format("01/02/06"),
		localReset.Format("15:04"),
		zone,
	)
}

func formatResetDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	minutes := int(duration.Round(time.Minute) / time.Minute)
	if minutes < 60 {
		return formatUsageDurationUnit(minutes, "minute")
	}
	hours := int(duration.Round(time.Hour) / time.Hour)
	if hours < 48 {
		return formatUsageDurationUnit(hours, "hour")
	}
	days := int(duration.Round(24*time.Hour) / (24 * time.Hour))
	return formatUsageDurationUnit(days, "day")
}

func formatUsageDurationUnit(value int, unit string) string {
	if value == 1 {
		return fmt.Sprintf("%d %s", value, unit)
	}
	return fmt.Sprintf("%d %ss", value, unit)
}

func usageNoticeRepeatKey(provider string, windowKey string, threshold float64) string {
	return fmt.Sprintf("%s:%s:%.2f", provider, windowKey, threshold)
}

func usageNoticeDue(
	policy config.AdapterNoticeRepeatPolicy,
	record usageNoticeRecord,
	notice UsageNotice,
	now time.Time,
) bool {
	if notice.ResetsAt.IsZero() || !record.ResetAt.Equal(notice.ResetsAt.UTC()) {
		return true
	}
	switch policy.Mode {
	case config.AdapterNoticeRepeatEvery:
		return true
	case config.AdapterNoticeRepeatOncePerThresholdUntilReset:
		return record.LastEmittedAt.IsZero()
	case config.AdapterNoticeRepeatTimeCooldown:
		return record.LastEmittedAt.IsZero() || !now.Before(record.LastEmittedAt.Add(policy.CooldownDuration))
	case config.AdapterNoticeRepeatTurnCooldown:
		return record.LastEmittedAt.IsZero() || record.TurnsSinceEmission >= policy.CooldownTurns
	default:
		return true
	}
}

// FormattedNoticeText wraps a notice body in the shared synthetic-notice
// envelope so Cursor BYOK renders it as a marker-bounded warning blockquote
// and so backend mappers strip it from the assistant transcript before
// reusing it on the next upstream turn. The leading blank line keeps the
// envelope visually separated from preceding answer text.
func FormattedNoticeText(text string) string {
	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, text)
	if envelope == "" {
		return ""
	}
	return "\n\n" + envelope
}

func noticeEvent(text string) (adapterrender.Event, bool) {
	formattedText := FormattedNoticeText(text)
	if formattedText == "" {
		return adapterrender.Event{EncryptedContent: ""}, false
	}
	return adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: formattedText, EncryptedContent: ""}, true
}

func EventsWithInjectedUsageNotices(ctx context.Context, events []adapterrender.Event, notices []UsageNotice) []adapterrender.Event {
	if len(notices) == 0 {
		return events
	}
	out := make([]adapterrender.Event, 0, len(events)+len(notices))
	out = append(out, events...)
	for _, notice := range notices {
		ev, ok := noticeEvent(notice.Text)
		if !ok {
			continue
		}
		LogNoticeEmission(ctx, notice, "stream_injected")
		out = append(out, ev)
	}
	return out
}

// LogNoticeEmission records the moment a usage notice is rendered into a
// chat surface so we can prove which provider/window/limit triggered any
// displayed warning. Backends that surface notices through their own paths
// should call this helper instead of reinventing the field set. Logged at
// debug per call so loops do not spam info-level events; the gate emits one
// info-level evaluate summary that bounds these per-emission lines.
func LogNoticeEmission(ctx context.Context, notice UsageNotice, surface string) {
	logger := slogger.WithConcern(slog.Default(), slogger.ConcernAdapterNotice)
	logger.LogAttrs(ctx, slog.LevelDebug, "adapter.notice.emitted",
		slog.String("component", "adapter"),
		slog.String("subcomponent", "usage_notice_gate"),
		slog.String("provider", notice.Provider),
		slog.String("window_key", notice.WindowKey),
		slog.String("limit_label", notice.LimitLabel),
		slog.String("kind", notice.Kind),
		slog.Float64("threshold", notice.Threshold),
		slog.Time("resets_at", notice.ResetsAt),
		slog.String("surface", surface),
	)
}

func AppendUsageNoticesToResponse(
	ctx context.Context,
	resp ChatResponse,
	notices []UsageNotice,
	encode encodeJSON,
) (ChatResponse, bool) {
	if len(notices) == 0 || len(resp.Choices) == 0 {
		return resp, false
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil {
		return resp, false
	}
	builder := strings.Builder{}
	for _, notice := range notices {
		formattedText := FormattedNoticeText(notice.Text)
		if formattedText == "" {
			continue
		}
		LogNoticeEmission(ctx, notice, "response_appended")
		builder.WriteString(formattedText)
	}
	if builder.Len() == 0 {
		return resp, false
	}
	content = builder.String() + content
	if encode == nil {
		encode = json.Marshal
	}
	encoded, err := encode(content)
	if err != nil {
		return resp, false
	}
	resp.Choices[0].Message.Content = json.RawMessage(encoded)
	return resp, true
}

func OpenAINoticeChunk(reqID string, modelAlias string, text string, includeRole bool) adapteropenai.StreamChunk {
	delta := adapteropenai.StreamDelta{Content: text}
	if includeRole {
		delta.Role = "assistant"
	}
	return adapteropenai.StreamChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: runtimeClock.Now().Unix(),
		Model:   modelAlias,
		Choices: []adapteropenai.StreamChoice{{
			Index: 0,
			Delta: delta,
		}},
	}
}
