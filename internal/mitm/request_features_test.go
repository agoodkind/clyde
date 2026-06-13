package mitm

import (
	"encoding/json"
	"testing"
)

// TestExtractRequestFeaturesModelID asserts the extractor pulls the model id
// from the captured request body and ignores every other field, which is the
// whole feature vector now that flavor selection matches on model alone.
func TestExtractRequestFeaturesModelID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        json.RawMessage
		wantModelID string
	}{
		{
			name:        "object body with ignored extra fields",
			body:        json.RawMessage(`{"model":"claude-fable-4-20250514","thinking":{"type":"adaptive"},"tools":[{"name":"Read"}]}`),
			wantModelID: "claude-fable-4-20250514",
		},
		{
			name:        "response format ignored",
			body:        json.RawMessage(`{"model":"claude-haiku-4-5-20251001","response_format":{"type":"json_object"}}`),
			wantModelID: "claude-haiku-4-5-20251001",
		},
		{
			name:        "string-wrapped json body",
			body:        mustJSONString(t, `{"model":"claude-opus-4-7"}`),
			wantModelID: "claude-opus-4-7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ExtractRequestFeatures(CapturedRequest{RequestBody: tc.body})
			if err != nil {
				t.Fatalf("ExtractRequestFeatures: %v", err)
			}
			want := RequestFeatures{ModelID: tc.wantModelID}
			if got != want {
				t.Fatalf("ExtractRequestFeatures() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestExtractRequestFeaturesRejectsNonObjectBody guards the decode error
// branches: empty, null, and non-object bodies are not extractable.
func TestExtractRequestFeaturesRejectsNonObjectBody(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", "null", `"not-json"`, `[1,2]`} {
		if _, err := ExtractRequestFeatures(CapturedRequest{RequestBody: json.RawMessage(body)}); err == nil {
			t.Fatalf("ExtractRequestFeatures(%q) error = nil, want non-nil", body)
		}
	}
}

func mustJSONString(t *testing.T, s string) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal json string: %v", err)
	}
	return raw
}
