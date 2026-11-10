package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseResponseFormatPlain(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, ""},
		{"text", `{"type":"text"}`, ""},
		{"json_object", `{"type":"json_object"}`, "json_object"},
		{"json_schema", `{"type":"json_schema","json_schema":{"name":"rename","schema":{"type":"object"}}}`, "json_schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseResponseFormat(json.RawMessage(c.raw))
			if got.Mode != c.want {
				t.Fatalf("Mode = %q, want %q", got.Mode, c.want)
			}
		})
	}
}

func TestSystemPromptIncludesSchema(t *testing.T) {
	spec := JSONResponseSpec{
		Mode:       "json_schema",
		SchemaName: "rename",
		Schema:     json.RawMessage(`{"type":"object","properties":{"newName":{"type":"string"}}}`),
	}
	got := spec.SystemPrompt(false)
	if !strings.Contains(got, "ONLY raw JSON") {
		t.Errorf("missing JSON-only directive: %q", got)
	}
	if !strings.Contains(got, `"newName"`) {
		t.Errorf("missing schema body: %q", got)
	}
	if !strings.Contains(got, "schema name: rename") {
		t.Errorf("missing schema name: %q", got)
	}
	retry := spec.SystemPrompt(true)
	if !strings.Contains(retry, "JSON.parse") {
		t.Errorf("retry hint missing: %q", retry)
	}
}

func TestCoerceJSONStripsFences(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"  prose before {\"x\":2} more after  ", `{"x":2}`},
		{"[1,2,3]", "[1,2,3]"},
		{"```\n[1,2]\n```", "[1,2]"},
	}
	for _, c := range cases {
		got := CoerceJSON(c.in)
		if got != c.want {
			t.Errorf("CoerceJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLooksLikeJSON(t *testing.T) {
	if !LooksLikeJSON(`{"a":1}`) {
		t.Error("expected valid object")
	}
	if LooksLikeJSON(`not json`) {
		t.Error("expected invalid")
	}
	if LooksLikeJSON(``) {
		t.Error("expected invalid for empty")
	}
}
