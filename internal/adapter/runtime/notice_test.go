package runtime

import (
	"encoding/json"
	"testing"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

func TestUsageNoticeGateFormatsWarningWithLocalResetTime(t *testing.T) {
	location := time.FixedZone("PDT", -7*60*60)
	now := time.Date(2026, time.May, 3, 18, 0, 0, 0, location)
	reset := time.Date(2026, time.May, 5, 17, 30, 0, 0, location)
	gate := NewUsageNoticeGate()

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

	want := "⚠️ You have about 7% of your weekly limit left. The limit resets in 2 days on 05/05/26 at 17:30 PDT."
	if notices[0].Text != want {
		t.Fatalf("notice text=%q want %q", notices[0].Text, want)
	}
}

func TestFormattedNoticeTextWrapsNoticeInBlockquote(t *testing.T) {
	got := FormattedNoticeText("⚠️ You have about 7% of your weekly limit left.")
	want := "\n\n> ⚠️ You have about 7% of your weekly limit left."
	if got != want {
		t.Fatalf("formatted notice=%q want %q", got, want)
	}
}

func TestAppendUsageNoticesToResponsePrependsBlockquote(t *testing.T) {
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
	want := "\n\n> ⚠️ Notice text.answer"
	if got != want {
		t.Fatalf("response content=%q want %q", got, want)
	}
}
