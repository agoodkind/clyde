package daemon

import (
	"fmt"
	"strings"
	"testing"
)

func TestEmbeddedLargeToolDisplayBoundary(t *testing.T) {
	var display strings.Builder
	for index := range 800 {
		fmt.Fprintf(&display, "program_%04d > output_%04d;\n", index, index)
	}
	message := embeddedOccurrenceToolMessage(display.String())
	message.Tools[0].DisplayLang = "bash"
	field := embeddedOccurrenceOnlyField(t, message)
	occurrences, err := embeddedFieldOccurrences(t.Context(), embeddedOccurrenceOwner(t, "/repo"), field)
	if err != nil {
		t.Fatalf("prepare %d-byte tool display with %d-byte prefix: %v", len(field.Text), len(field.DocumentPrefix), err)
	}
	if len(occurrences) < 2 {
		t.Fatalf("occurrences = %d, want multiple source spans", len(occurrences))
	}
	var covered strings.Builder
	for index, occurrence := range occurrences {
		if len(occurrence.EmbeddingInput) > embeddedOccurrenceInputLimitBytes {
			t.Fatalf("part %d has %d input bytes", index, len(occurrence.EmbeddingInput))
		}
		if !strings.HasPrefix(occurrence.EmbeddingInput, embeddedOccurrenceToolName+"\n") {
			t.Fatalf("part %d lacks tool attribution", index)
		}
		covered.WriteString(occurrence.SourceText)
	}
	if covered.String() != display.String() {
		t.Fatalf("source spans cover %d of %d original bytes", covered.Len(), display.Len())
	}
}
