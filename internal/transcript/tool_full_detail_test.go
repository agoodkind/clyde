package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShapeConversationUsesToolFullDetailDisplay(t *testing.T) {
	t.Parallel()

	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools: []ToolCall{{
			Name:        "Read",
			Input:       ToolInputJSON{Raw: json.RawMessage(`{"file_path":"/tmp/probe.log"}`)},
			Display:     "/tmp/probe.log",
			DisplayLang: "",
			Output:      "",
			IsError:     false,
		}},
	}}, ShapeOptions{ToolOnly: ToolOnlyFullDetail})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if strings.Contains(turns[0].Text, "file_path") {
		t.Fatalf("text=%q unexpectedly contained raw input json", turns[0].Text)
	}
	want := "[tool: Read] /tmp/probe.log"
	if turns[0].Text != want {
		t.Fatalf("text=%q want %q", turns[0].Text, want)
	}
}

func TestShapeConversationUsesToolFullDetailFallbackName(t *testing.T) {
	t.Parallel()

	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools: []ToolCall{{
			Name:        "Mystery",
			Input:       ToolInputJSON{Raw: json.RawMessage(`{"other":"value"}`)},
			Display:     "",
			DisplayLang: "",
			Output:      "",
			IsError:     false,
		}},
	}}, ShapeOptions{ToolOnly: ToolOnlyFullDetail})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	want := "[tool: Mystery]"
	if turns[0].Text != want {
		t.Fatalf("text=%q want %q", turns[0].Text, want)
	}
}
