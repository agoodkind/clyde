package compat

import (
	"encoding/json"
	"strings"
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

func TestPresenceDistinguishesAllClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  json.RawMessage
		want fieldPresence
	}{
		{name: "absent_nil", raw: nil, want: presenceAbsent},
		{name: "absent_empty", raw: json.RawMessage(""), want: presenceAbsent},
		{name: "null", raw: json.RawMessage("null"), want: presenceNull},
		{name: "empty_string", raw: json.RawMessage(`""`), want: presenceEmpty},
		{name: "empty_array", raw: json.RawMessage("[]"), want: presenceEmpty},
		{name: "empty_object", raw: json.RawMessage("{}"), want: presenceEmpty},
		{name: "zero", raw: json.RawMessage("0"), want: presenceZero},
		{name: "false_present", raw: json.RawMessage("false"), want: presencePresent},
		{name: "number_present", raw: json.RawMessage("0.5"), want: presencePresent},
		{name: "string_present", raw: json.RawMessage(`"x"`), want: presencePresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := presence(tc.raw); got != tc.want {
				t.Fatalf("presence(%q) = %d, want %d", string(tc.raw), got, tc.want)
			}
		})
	}
}

func warningParams(warnings []CompatibilityWarning) []string {
	params := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		params = append(params, warning.Param)
	}
	return params
}

func TestComputeWarningsCodexOmittedFieldsInCanonicalOrder(t *testing.T) {
	t.Parallel()
	// prompt_cache_retention is placed first in the body so raw key order
	// cannot leak into the warning order; the catalog order must win.
	body := []byte(`{"prompt_cache_retention":"24h","stop":["x"],"top_p":0.9,"temperature":0.5,"max_output_tokens":10,"model":"gpt"}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	if set.Empty() {
		t.Fatalf("expected warnings, got none")
	}
	got := warningParams(set.Slice())
	want := []string{"max_output_tokens", "temperature", "top_p", "stop", "prompt_cache_retention"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("params = %v, want %v", got, want)
	}
	for _, warning := range set.Slice() {
		if warning.Code != "field_omitted" || warning.Disposition != "omitted" {
			t.Fatalf("warning = %+v, want field_omitted/omitted", warning)
		}
		if strings.Contains(warning.Message, "0.5") || strings.Contains(warning.Message, "10") {
			t.Fatalf("warning message leaked a value: %q", warning.Message)
		}
	}
}

func TestComputeWarningsAnthropicOmittedFields(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude","include":["reasoning.encrypted_content"],"service_tier":"flex"}`)
	for _, provider := range []adaptermodel.BackendID{adaptermodel.BackendAnthropic, adaptermodel.BackendClaude} {
		set := ComputeWarnings(body, provider, EndpointResponses)
		got := warningParams(set.Slice())
		want := []string{"include", "service_tier"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("provider %s params = %v, want %v", provider, got, want)
		}
		for _, warning := range set.Slice() {
			if !strings.Contains(warning.Message, "anthropic") {
				t.Fatalf("provider %s message = %q, want anthropic backend", provider, warning.Message)
			}
		}
	}
}

func TestComputeWarningsStoreOverrideForCodex(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","store":true}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	slice := set.Slice()
	if len(slice) != 1 {
		t.Fatalf("warnings = %v, want one store override", slice)
	}
	warning := slice[0]
	if warning.Param != "store" || warning.Code != "field_overridden" || warning.Disposition != "overridden" {
		t.Fatalf("store warning = %+v", warning)
	}
}

func TestComputeWarningsStoreOmittedForAnthropic(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude","store":true}`)
	set := ComputeWarnings(body, adaptermodel.BackendAnthropic, EndpointResponses)
	slice := set.Slice()
	if len(slice) != 1 || slice[0].Param != "store" || slice[0].Code != "field_omitted" {
		t.Fatalf("store warning = %+v, want field_omitted", slice)
	}
}

func TestComputeWarningsNullTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","temperature":null}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	if !set.Empty() {
		t.Fatalf("null temperature warned: %v", set.Slice())
	}
}

func TestComputeWarningsZeroTemperatureWarns(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","temperature":0}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	slice := set.Slice()
	if len(slice) != 1 || slice[0].Param != "temperature" {
		t.Fatalf("zero temperature warnings = %v, want temperature", slice)
	}
}

func TestComputeWarningsPassthroughYieldsNone(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","temperature":0.5,"store":true,"include":["a"]}`)
	set := ComputeWarnings(body, adaptermodel.BackendPassthroughOverride, EndpointResponses)
	if !set.Empty() {
		t.Fatalf("passthrough warned: %v", set.Slice())
	}
}

func TestComputeWarningsChatEndpointYieldsNone(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","temperature":0.5}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointChat)
	if !set.Empty() {
		t.Fatalf("chat endpoint warned: %v", set.Slice())
	}
}

func TestComputeWarningsTranslateFieldsNeverWarn(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","input":"hi","instructions":"be terse","tools":[],"metadata":{},"parallel_tool_calls":true}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	if !set.Empty() {
		t.Fatalf("translate-only body warned: %v", set.Slice())
	}
}

func TestHeadersAreCompactJSONPerWarning(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt","temperature":0.5,"top_p":0.9}`)
	set := ComputeWarnings(body, adaptermodel.BackendCodex, EndpointResponses)
	headers := set.Headers()
	if len(headers) != 2 {
		t.Fatalf("headers = %v, want two", headers)
	}
	for _, header := range headers {
		if strings.Contains(header, "\n") || strings.Contains(header, `": `) || strings.Contains(header, `", `) {
			t.Fatalf("header not compact: %q", header)
		}
		var warning CompatibilityWarning
		if err := json.Unmarshal([]byte(header), &warning); err != nil {
			t.Fatalf("header %q not valid JSON: %v", header, err)
		}
		if warning.Param == "" {
			t.Fatalf("header %q missing param", header)
		}
	}
}

func TestEmptySetHasNoHeaders(t *testing.T) {
	t.Parallel()
	set := ComputeWarnings([]byte(`{"model":"gpt"}`), adaptermodel.BackendCodex, EndpointResponses)
	if !set.Empty() {
		t.Fatalf("unexpected warnings: %v", set.Slice())
	}
	if headers := set.Headers(); len(headers) != 0 {
		t.Fatalf("empty set headers = %v, want none", headers)
	}
}

func TestDedupWarningsByCodeAndParam(t *testing.T) {
	t.Parallel()
	in := []CompatibilityWarning{
		{Code: "field_omitted", Param: "temperature", Disposition: "omitted", Message: "a"},
		{Code: "field_omitted", Param: "temperature", Disposition: "omitted", Message: "b"},
		{Code: "field_omitted", Param: "top_p", Disposition: "omitted", Message: "c"},
	}
	out := dedupWarnings(in)
	if len(out) != 2 {
		t.Fatalf("dedup len = %d, want 2 (%v)", len(out), out)
	}
	if out[0].Param != "temperature" || out[0].Message != "a" || out[1].Param != "top_p" {
		t.Fatalf("dedup kept wrong entries: %v", out)
	}
}

func TestCapWarningsEnforcesCountCap(t *testing.T) {
	t.Parallel()
	in := make([]CompatibilityWarning, 0, maxWarnings+5)
	for i := 0; i < maxWarnings+5; i++ {
		in = append(in, CompatibilityWarning{
			Code:        "field_omitted",
			Param:       "p" + strings.Repeat("x", i%3),
			Disposition: "omitted",
			Message:     "m",
		})
	}
	out := capWarnings(in)
	if len(out) > maxWarnings {
		t.Fatalf("cap len = %d, want at most %d", len(out), maxWarnings)
	}
}

func TestCapWarningsEnforcesByteBudget(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("z", 2000)
	in := make([]CompatibilityWarning, 0, 10)
	for i := 0; i < 10; i++ {
		in = append(in, CompatibilityWarning{
			Code:        "field_omitted",
			Param:       "temperature",
			Disposition: "omitted",
			Message:     long,
		})
	}
	out := capWarnings(in)
	total := 0
	for _, warning := range out {
		total += len(warningHeader(warning))
	}
	if total > maxHeaderBytes {
		t.Fatalf("total header bytes = %d, want at most %d", total, maxHeaderBytes)
	}
	if len(out) >= len(in) {
		t.Fatalf("byte budget did not drop any overflow: kept %d of %d", len(out), len(in))
	}
}
