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

func TestResponsesFieldSetClassifiesRawJSONTokens(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
		want  ResponsesFieldPresence
	}{
		{name: "zero", body: `{"model":"gpt","temperature":0}`, field: "temperature", want: ResponsesFieldZero},
		{name: "decimal zero", body: `{"model":"gpt","temperature":0.0}`, field: "temperature", want: ResponsesFieldZero},
		{name: "negative decimal zero", body: `{"model":"gpt","temperature":-0.0}`, field: "temperature", want: ResponsesFieldZero},
		{name: "exponent zero", body: `{"model":"gpt","temperature":0e5}`, field: "temperature", want: ResponsesFieldZero},
		{name: "decimal exponent zero", body: `{"model":"gpt","temperature":0.00E-2}`, field: "temperature", want: ResponsesFieldZero},
		{name: "quoted zero", body: `{"model":"gpt","input":"0"}`, field: "input", want: ResponsesFieldPresent},
		{name: "quoted decimal zero", body: `{"model":"gpt","input":"0.0"}`, field: "input", want: ResponsesFieldPresent},
		{name: "quoted exponent zero", body: `{"model":"gpt","input":"-0e5"}`, field: "input", want: ResponsesFieldPresent},
		{name: "empty string", body: `{"model":"gpt","input":""}`, field: "input", want: ResponsesFieldEmpty},
		{name: "nonzero decimal", body: `{"model":"gpt","temperature":0.1}`, field: "temperature", want: ResponsesFieldPresent},
		{name: "negative", body: `{"model":"gpt","temperature":-1}`, field: "temperature", want: ResponsesFieldPresent},
		{name: "null", body: `{"model":"gpt","input":null}`, field: "input", want: ResponsesFieldNull},
		{name: "false", body: `{"model":"gpt","store":false}`, field: "store", want: ResponsesFieldFalse},
		{name: "empty array", body: `{"model":"gpt","input":[]}`, field: "input", want: ResponsesFieldEmpty},
		{name: "empty object", body: `{"model":"gpt","input":{}}`, field: "input", want: ResponsesFieldEmpty},
		{name: "absent", body: `{"model":"gpt"}`, field: "input", want: ResponsesFieldAbsent},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, err := UnmarshalResponsesRequest([]byte(test.body))
			if err != nil {
				t.Fatalf("UnmarshalResponsesRequest: %v", err)
			}
			if got := request.Fields.Presence(test.field); got != test.want {
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
