package openai

import (
	"reflect"
	"strings"
	"testing"
)

// TestChatRequestJSONTagsMatchKnownKeys is the drift check discovery.go
// asks for: the hand-maintained knownChatRequestKeys catalog must equal
// the set of JSON keys ChatRequest actually serializes. The
// compatibility field-disposition catalog translates, warns on, or
// rejects exactly these keys, so a new or renamed ChatRequest field that
// is not added to knownChatRequestKeys, or a stale catalog entry with no
// backing field, both fail here. The set is compared in both directions.
func TestChatRequestJSONTagsMatchKnownKeys(t *testing.T) {
	actual := chatRequestJSONTags()
	for key := range actual {
		if !knownChatRequestKeys[key] {
			t.Errorf("ChatRequest serializes JSON key %q absent from knownChatRequestKeys; add it to the catalog in discovery.go", key)
		}
	}
	for key := range knownChatRequestKeys {
		if !actual[key] {
			t.Errorf("knownChatRequestKeys lists %q but ChatRequest no longer serializes that JSON key", key)
		}
	}
}

// The OpenAI listener rejects every Responses key missing from
// knownResponsesRequestKeys. This test fails when that set differs from the
// ResponsesRequest JSON tags.
func TestResponsesRequestJSONTagsMatchKnownKeys(t *testing.T) {
	actual := structJSONTags(reflect.TypeOf(ResponsesRequest{}))
	for key := range actual {
		if !knownResponsesRequestKeys[key] {
			t.Errorf("ResponsesRequest serializes JSON key %q absent from knownResponsesRequestKeys", key)
		}
	}
	for key := range knownResponsesRequestKeys {
		if !actual[key] {
			t.Errorf("knownResponsesRequestKeys lists %q but ResponsesRequest no longer serializes that JSON key", key)
		}
	}
}

// This test fails when knownCompletionRequestKeys differs from the
// CompletionRequest JSON tags.
func TestCompletionRequestJSONTagsMatchKnownKeys(t *testing.T) {
	actual := structJSONTags(reflect.TypeOf(CompletionRequest{}))
	for key := range actual {
		if !knownCompletionRequestKeys[key] {
			t.Errorf("CompletionRequest serializes JSON key %q absent from knownCompletionRequestKeys", key)
		}
	}
	for key := range knownCompletionRequestKeys {
		if !actual[key] {
			t.Errorf("knownCompletionRequestKeys lists %q but CompletionRequest no longer serializes that JSON key", key)
		}
	}
}

// chatRequestJSONTags reflects the ChatRequest struct into the set of
// JSON key names it serializes, dropping the omitempty suffix and any
// field tagged json:"-".
func chatRequestJSONTags() map[string]bool {
	return structJSONTags(reflect.TypeOf(ChatRequest{}))
}

func structJSONTags(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	for fieldIndex := 0; fieldIndex < typ.NumField(); fieldIndex++ {
		tag := typ.Field(fieldIndex).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		out[name] = true
	}
	return out
}
