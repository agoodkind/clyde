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

// TestResponsesRequestJSONTagsMatchKnownKeys is the same drift check for
// the Responses request. The OpenAI listener rejects every key absent
// from knownResponsesRequestKeys. The catalog must equal the struct tags.
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

// chatRequestJSONTags reflects the ChatRequest struct into the set of
// JSON key names it serializes, dropping the omitempty suffix and any
// field tagged json:"-".
func chatRequestJSONTags() map[string]bool {
	return structJSONTags(reflect.TypeOf(ChatRequest{}))
}

// structJSONTags reflects a struct type into the set of JSON key names it
// serializes.
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
