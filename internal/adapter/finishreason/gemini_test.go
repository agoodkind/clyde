package finishreason_test

import (
	"testing"

	"goodkind.io/clyde/internal/adapter/finishreason"
)

func TestGeminiFinishReasons(t *testing.T) {
	for input, want := range map[string]string{
		"":                        "stop",
		"STOP":                    "stop",
		"OTHER":                   "stop",
		"MAX_TOKENS":              "length",
		"SAFETY":                  "content_filter",
		"RECITATION":              "content_filter",
		"MALFORMED_FUNCTION_CALL": "stop",
		"UNEXPECTED_TOOL_CALL":    "stop",
	} {
		if got := finishreason.FromGemini(input); got != want {
			t.Errorf("FromGemini(%q) = %q, want %q", input, got, want)
		}
	}
}
