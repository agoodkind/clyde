package runtime

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/config"
)

// joinTextParts concatenates the text parts produced by
// [adapterrender.ExtractSyntheticParts]. The runtime tests use it to assert
// that synthetic envelopes are removed from a rendered string while the
// non-envelope text is preserved.
func joinTextParts(parts []adapterrender.SyntheticPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind != adapterrender.SyntheticKindText {
			continue
		}
		b.WriteString(p.Body)
	}
	return b.String()
}

func TestUsageNoticeGateFormatsWarningWithLocalResetTime(t *testing.T) {
	location := time.FixedZone("PDT", -7*60*60)
	now := time.Date(2026, time.May, 3, 18, 0, 0, 0, location)
	reset := time.Date(2026, time.May, 5, 17, 30, 0, 0, location)
	gate := NewUsageNoticeGateWithLogger(nil)

	notices := gate.Evaluate([]UsageWindowNoticeInput{{
		Provider:    "codex",
		WindowKey:   "weekly",
		LimitLabel:  "weekly",
		UsedPercent: 93,
		ResetsAt:    reset,
	}}, config.AdapterNotices{}, now)
	if len(notices) != 1 {
		t.Fatalf("notices len=%d want 1", len(notices))
	}

	want := "⚠️ Codex: You have about 7% of your weekly limit left. The limit resets in 2 days on 05/05/26 at 17:30 PDT."
	if notices[0].Text != want {
		t.Fatalf("notice text=%q want %q", notices[0].Text, want)
	}
	if notices[0].Provider != "codex" {
		t.Fatalf("notice provider=%q want codex", notices[0].Provider)
	}
	if notices[0].LimitLabel != "weekly" {
		t.Fatalf("notice limit_label=%q want weekly", notices[0].LimitLabel)
	}
}

func TestUsageNoticeGateLogsProviderAndWindowKey(t *testing.T) {
	location := time.FixedZone("PDT", -7*60*60)
	now := time.Date(2026, time.May, 3, 18, 0, 0, 0, location)
	reset := time.Date(2026, time.May, 5, 17, 30, 0, 0, location)

	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gate := NewUsageNoticeGateWithLogger(logger)

	notices := gate.Evaluate([]UsageWindowNoticeInput{{
		Provider:    "codex",
		WindowKey:   "weekly",
		LimitLabel:  "weekly",
		UsedPercent: 93,
		ResetsAt:    reset,
	}}, config.AdapterNotices{}, now)
	if len(notices) != 1 {
		t.Fatalf("notices len=%d want 1", len(notices))
	}

	out := buf.String()
	for _, fragment := range []string{
		`"msg":"adapter.notice.evaluated"`,
		`"provider":"codex"`,
		`"window_key":"weekly"`,
		`"limit_label":"weekly"`,
		`"decision":"due"`,
	} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("per-window notice log missing %s in %q", fragment, out)
		}
	}
	for _, fragment := range []string{
		`"msg":"adapter.notice.evaluate_summary"`,
		`"due_count":1`,
		`"providers":["codex"]`,
		`"window_keys":["weekly"]`,
		`"limit_labels":["weekly"]`,
	} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("summary notice log missing %s in %q", fragment, out)
		}
	}
}

func TestFormattedNoticeTextWrapsNoticeInSyntheticEnvelope(t *testing.T) {
	got := FormattedNoticeText("⚠️ You have about 7% of your weekly limit left.")
	if !strings.HasPrefix(got, "\n\n") {
		t.Fatalf("missing leading separator: %q", got)
	}
	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "⚠️ You have about 7% of your weekly limit left.")
	if got != "\n\n"+envelope {
		t.Fatalf("formatted notice=%q want leading blank line plus synthetic envelope %q", got, envelope)
	}
	if rest := joinTextParts(adapterrender.ExtractSyntheticParts(got)); strings.TrimSpace(rest) != "" {
		t.Fatalf("text-only join should be empty whitespace, got %q", rest)
	}
}

func TestAppendUsageNoticesToResponsePrependsSyntheticNotice(t *testing.T) {
	content, err := json.Marshal("answer")
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	response := ChatResponse{
		Choices: []adapteropenai.ChatChoice{{
			Message: adapteropenai.ChatMessage{Content: content},
		}},
	}

	updated, ok := AppendUsageNoticesToResponse(response, []UsageNotice{{Text: "⚠️ Notice text."}}, json.Marshal)
	if !ok {
		t.Fatalf("AppendUsageNoticesToResponse did not update response")
	}
	var got string
	if err := json.Unmarshal(updated.Choices[0].Message.Content, &got); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if !strings.Contains(got, "<!--clyde-notice-->") || !strings.Contains(got, "<!--/clyde-notice-->") {
		t.Fatalf("response missing notice markers: %q", got)
	}
	if !strings.Contains(got, "⚠️ Notice text.") {
		t.Fatalf("response missing notice body: %q", got)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "answer") {
		t.Fatalf("answer should remain at end: %q", got)
	}
	stripped := joinTextParts(adapterrender.ExtractSyntheticParts(got))
	if strings.TrimSpace(stripped) != "answer" {
		t.Fatalf("text-only join should leave only the answer, got %q", stripped)
	}
}
