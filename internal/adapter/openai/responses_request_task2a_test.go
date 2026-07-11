package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesRequestJSONTagsMatchCurrentOfficialFields(t *testing.T) {
	want := []string{
		"previous_response_id", "model", "background", "max_tool_calls", "text", "tools", "tool_choice", "prompt", "prompt_cache_options", "top_logprobs", "metadata", "temperature", "top_p", "user", "safety_identifier", "prompt_cache_key", "service_tier", "prompt_cache_retention", "truncation", "reasoning", "input", "include", "parallel_tool_calls", "store", "instructions", "moderation", "stream", "stream_options", "conversation", "context_management", "max_output_tokens", "max_tokens", "max_completion_tokens", "n", "stop",
	}
	got := responsesRequestJSONTags()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ResponsesRequest JSON tags = %v, want %v", got, want)
	}
}

func TestResponsesFieldSetDistinguishesAllPresentForms(t *testing.T) {
	request, err := UnmarshalResponsesRequest([]byte(`{"model":"gpt","background":null,"text":{},"store":false,"max_tool_calls":0,"input":"hello"}`))
	if err != nil {
		t.Fatalf("UnmarshalResponsesRequest: %v", err)
	}
	cases := []struct {
		field string
		want  ResponsesFieldPresence
	}{
		{field: "previous_response_id", want: ResponsesFieldAbsent},
		{field: "background", want: ResponsesFieldNull},
		{field: "text", want: ResponsesFieldEmpty},
		{field: "store", want: ResponsesFieldFalse},
		{field: "max_tool_calls", want: ResponsesFieldZero},
		{field: "input", want: ResponsesFieldPresent},
	}
	for _, tc := range cases {
		if got := request.Fields.Presence(tc.field); got != tc.want {
			t.Errorf("%s presence = %v, want %v", tc.field, got, tc.want)
		}
	}
}

func TestResponsesFieldSetRecognizesNumericZeroSpellings(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ResponsesFieldPresence
	}{
		{name: "zero", raw: "0", want: ResponsesFieldZero},
		{name: "decimal zero", raw: "0.0", want: ResponsesFieldZero},
		{name: "negative decimal zero", raw: "-0.0", want: ResponsesFieldZero},
		{name: "exponent zero", raw: "0e5", want: ResponsesFieldZero},
		{name: "decimal exponent zero", raw: "0.00E-2", want: ResponsesFieldZero},
		{name: "positive", raw: "0.1", want: ResponsesFieldPresent},
		{name: "negative", raw: "-1", want: ResponsesFieldPresent},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, err := UnmarshalResponsesRequest([]byte(`{"model":"gpt","temperature":` + test.raw + `}`))
			if err != nil {
				t.Fatalf("UnmarshalResponsesRequest: %v", err)
			}
			if got := request.Fields.Presence("temperature"); got != test.want {
				t.Fatalf("presence = %v, want %v", got, test.want)
			}
		})
	}
}

func responsesRequestJSONTags() []string {
	typ := reflect.TypeOf(ResponsesRequest{})
	got := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		tag := typ.Field(index).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			got = append(got, name)
		}
	}
	return got
}

var _ json.Unmarshaler = (*ResponsesRequest)(nil)
