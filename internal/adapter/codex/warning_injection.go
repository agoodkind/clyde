package codex

import (
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func warningInjectingEventWriter(writer interface {
	WriteEvent(adapterrender.Event) error
}, warningText string) func(adapterrender.Event) error {
	formattedWarning := formatUsageWarningNotice(warningText)
	warningEmitted := false
	return func(ev adapterrender.Event) error {
		if !warningEmitted && formattedWarning != "" && ev.Kind == adapterrender.EventAssistantTextDelta {
			warningEmitted = true
			if err := writer.WriteEvent(adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: formattedWarning}); err != nil {
				return err
			}
		}
		return writer.WriteEvent(ev)
	}
}

func EventsWithInjectedUsageWarning(events []adapterrender.Event, warningText string) []adapterrender.Event {
	formattedWarning := formatUsageWarningNotice(warningText)
	if formattedWarning == "" {
		return events
	}
	for index, event := range events {
		if event.Kind != adapterrender.EventAssistantTextDelta {
			continue
		}
		out := make([]adapterrender.Event, 0, len(events)+1)
		out = append(out, events[:index]...)
		out = append(out, adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: formattedWarning})
		out = append(out, events[index:]...)
		return out
	}
	return events
}

func firstUsageWarningText(warnings []UsageWarning) string {
	if len(warnings) == 0 {
		return ""
	}
	return strings.TrimSpace(warnings[0].Text)
}

func formatUsageWarningNotice(text string) string {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return ""
	}
	return "<!--clyde-notice--><blockquote><small>" + trimmedText + "</small></blockquote><!--/clyde-notice-->\n\n"
}
