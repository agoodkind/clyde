package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/config"
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
	Kind      string
	Text      string
	ResetsAt  time.Time
	Threshold float64
	WindowKey string
}

type usageNoticeRecord struct {
	ResetAt            time.Time
	LastEmittedAt      time.Time
	TurnsSinceEmission int
}

type UsageNoticeGate struct {
	mu      sync.Mutex
	records map[string]usageNoticeRecord
}

func NewUsageNoticeGate() *UsageNoticeGate {
	return &UsageNoticeGate{records: make(map[string]usageNoticeRecord)}
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

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.records == nil {
		g.records = make(map[string]usageNoticeRecord)
	}

	due := make([]UsageNotice, 0, len(windows))
	for _, window := range windows {
		notice, ok := evaluateUsageWindow(window, thresholds, now)
		if !ok {
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
			continue
		}
		record.ResetAt = notice.ResetsAt.UTC()
		record.TurnsSinceEmission++
		g.records[key] = record
	}
	return due
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
		Kind:      kind,
		Text:      usageNoticeText(usageRemainingPercent(window.UsedPercent), limitLabel, window.ResetsAt, now),
		ResetsAt:  window.ResetsAt.UTC(),
		Threshold: highestThreshold,
		WindowKey: windowKey,
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

func usageNoticeText(remainingPercent int, limitLabel string, resetsAt time.Time, now time.Time) string {
	return fmt.Sprintf(
		"⚠️ You have about %d%% of your %s limit left. The limit resets %s.",
		remainingPercent,
		limitLabel,
		usageResetPhrase(resetsAt, now),
	)
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

func FormattedNoticeText(text string) string {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return ""
	}
	return "\n\n> " + trimmedText
}

func noticeEvent(text string) (adapterrender.Event, bool) {
	formattedText := FormattedNoticeText(text)
	if formattedText == "" {
		return adapterrender.Event{}, false
	}
	return adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: formattedText}, true
}

func EventsWithInjectedUsageNotices(events []adapterrender.Event, notices []UsageNotice) []adapterrender.Event {
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
		out = append(out, ev)
	}
	return out
}

func AppendUsageNoticesToResponse(
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

func OpenAINoticeChunk(reqID string, modelAlias string, text string) adapteropenai.StreamChunk {
	return adapteropenai.StreamChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: runtimeClock.Now().Unix(),
		Model:   modelAlias,
		Choices: []adapteropenai.StreamChoice{{
			Index: 0,
			Delta: adapteropenai.StreamDelta{
				Role:    "assistant",
				Content: text,
			},
		}},
	}
}
