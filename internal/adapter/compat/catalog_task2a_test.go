package compat

import (
	"strings"
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

func TestResponsesCatalogHasEveryOfficialFieldAndExtensionInOrder(t *testing.T) {
	want := []string{
		"previous_response_id", "model", "background", "max_tool_calls", "text", "tools", "tool_choice", "prompt", "prompt_cache_options", "top_logprobs", "metadata", "temperature", "top_p", "user", "safety_identifier", "prompt_cache_key", "service_tier", "prompt_cache_retention", "truncation", "reasoning", "input", "include", "parallel_tool_calls", "store", "instructions", "moderation", "stream", "stream_options", "conversation", "context_management", "max_output_tokens", "max_tokens", "max_completion_tokens", "n", "stop",
	}
	got := make([]string, 0, len(responsesCatalog))
	for _, entry := range responsesCatalog {
		got = append(got, entry.param)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("catalog params = %v, want %v", got, want)
	}
	for _, entry := range responsesCatalog {
		if entry.codex < dispositionTranslate || entry.codex > dispositionPartial || entry.anthropic < dispositionTranslate || entry.anthropic > dispositionPartial {
			t.Fatalf("catalog entry %q has incomplete dispositions: %+v", entry.param, entry)
		}
	}
}

func TestResponsesCatalogRejectsUnavailableOpenAIReferences(t *testing.T) {
	for _, param := range []string{"previous_response_id", "prompt", "conversation"} {
		found := false
		for _, entry := range responsesCatalog {
			if entry.param == param && entry.codex == dispositionReject && entry.anthropic == dispositionReject {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was not rejected for both providers", param)
		}
	}
}

func TestResponsesNWarnsOnlyAboveOne(t *testing.T) {
	presence := func(param string) int {
		if param == "n" {
			return 5
		}
		return 0
	}
	for _, n := range []*int{nil, new(int)} {
		if got := ComputeWarningsFromResponsesPresence(presence, n, adaptermodel.BackendCodex, EndpointResponses, nil); !got.Empty() {
			t.Fatalf("n=%v warnings=%v", n, got.Slice())
		}
	}
	n := 2
	set := ComputeWarningsFromResponsesPresence(presence, &n, adaptermodel.BackendCodex, EndpointResponses, nil)
	if len(set.Slice()) != 1 || set.Slice()[0].Param != "n" {
		t.Fatalf("n=2 warnings=%v", set.Slice())
	}
}
