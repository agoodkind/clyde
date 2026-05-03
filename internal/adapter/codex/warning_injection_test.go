package codex

import (
	"strings"
	"testing"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestEventsWithInjectedUsageWarningPrependsBeforeAssistantText(t *testing.T) {
	events := []adapterrender.Event{
		{Kind: adapterrender.EventReasoningDelta, Text: "thinking"},
		{Kind: adapterrender.EventAssistantTextDelta, Text: "answer"},
	}
	updated := EventsWithInjectedUsageWarning(events, "Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown.")
	if len(updated) != 3 {
		t.Fatalf("events len=%d want 3", len(updated))
	}
	if got := updated[1].Text; got != formatUsageWarningNotice("Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown.") {
		t.Fatalf("warning=%q", got)
	}
	if got := updated[2].Text; got != "answer" {
		t.Fatalf("assistant=%q", got)
	}
}

func TestWarningInjectingEventWriterQueuesWarningUntilAssistantText(t *testing.T) {
	writer := &capturingWriter{}
	emit := warningInjectingEventWriter(writer, "Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown.")
	if err := emit(adapterrender.Event{Kind: adapterrender.EventReasoningDelta, Text: "thinking"}); err != nil {
		t.Fatalf("emit reasoning: %v", err)
	}
	if len(writer.events) != 1 {
		t.Fatalf("events len=%d want 1", len(writer.events))
	}
	if err := emit(adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: "answer"}); err != nil {
		t.Fatalf("emit assistant: %v", err)
	}
	if len(writer.events) != 3 {
		t.Fatalf("events len=%d want 3", len(writer.events))
	}
	if !strings.Contains(writer.events[1].Text, "less than 25% of your weekly limit left") {
		t.Fatalf("warning=%q", writer.events[1].Text)
	}
	if writer.events[2].Text != "answer" {
		t.Fatalf("assistant=%q", writer.events[2].Text)
	}
}
