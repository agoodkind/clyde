package cursor

import (
	"encoding/json"
	"reflect"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func toolWithName(name string) adapteropenai.Tool {
	return adapteropenai.Tool{
		Type: "function",
		Function: adapteropenai.ToolFunctionSchema{
			Name: name,
		},
	}
}

func TestTranslateRequestSetsMetadataFields(t *testing.T) {
	meta := json.RawMessage(`{"cursorConversationId":"conv-abc","cursorRequestId":"req-xyz"}`)
	req := adapteropenai.ChatRequest{
		Model:    "gpt-5.3-codex",
		Messages: []adapteropenai.ChatMessage{{Role: "user"}},
		Metadata: meta,
	}
	got := TranslateRequest(req)
	if got.ConversationID != "conv-abc" {
		t.Errorf("ConversationID = %q, want conv-abc", got.ConversationID)
	}
	if got.RequestID != "req-xyz" {
		t.Errorf("RequestID = %q, want req-xyz", got.RequestID)
	}
}

func TestTranslateRequestPopulatesToolPresenceFlags(t *testing.T) {
	req := adapteropenai.ChatRequest{
		Model: "gpt-5.3-codex",
		Tools: []adapteropenai.Tool{
			toolWithName("ReadFile"),
			toolWithName("Task"),
			toolWithName("SwitchMode"),
			toolWithName("AskQuestion"),
			toolWithName("CreatePlan"),
			toolWithName("ApplyPatch"),
		},
	}
	got := TranslateRequest(req)
	if !got.HasSubagentTool {
		t.Errorf("HasSubagentTool = false, want true")
	}
	if !got.HasSwitchModeTool {
		t.Errorf("HasSwitchModeTool = false, want true")
	}
	if !got.HasAskQuestionTool {
		t.Errorf("HasAskQuestionTool = false, want true")
	}
	if !got.HasCreatePlanTool {
		t.Errorf("HasCreatePlanTool = false, want true")
	}
	if !got.HasApplyPatchTool {
		t.Errorf("HasApplyPatchTool = false, want true")
	}
}

func TestTranslateRequestAbsentToolsAreFalse(t *testing.T) {
	req := adapteropenai.ChatRequest{
		Model: "gpt-5.3-codex",
		Tools: []adapteropenai.Tool{toolWithName("ReadFile")},
	}
	got := TranslateRequest(req)
	if got.HasSubagentTool {
		t.Errorf("HasSubagentTool = true, want false")
	}
	if got.HasSwitchModeTool {
		t.Errorf("HasSwitchModeTool = true, want false")
	}
	if got.HasAskQuestionTool {
		t.Errorf("HasAskQuestionTool = true, want false")
	}
	if got.HasCreatePlanTool {
		t.Errorf("HasCreatePlanTool = true, want false")
	}
	if got.HasApplyPatchTool {
		t.Errorf("HasApplyPatchTool = true, want false")
	}
}

func TestTranslateRequestCollectsMCPToolNames(t *testing.T) {
	req := adapteropenai.ChatRequest{
		Tools: []adapteropenai.Tool{
			toolWithName("CallMcpTool"),
			toolWithName("FetchMcpResource"),
			toolWithName("ReadFile"),
			toolWithName("custom_mcp_thing"),
		},
	}
	got := TranslateRequest(req)
	want := []string{"CallMcpTool", "FetchMcpResource", "custom_mcp_thing"}
	if !reflect.DeepEqual(got.MCPToolNames, want) {
		t.Errorf("MCPToolNames = %v, want %v", got.MCPToolNames, want)
	}
}

func TestTranslateRequestNoToolsLeavesMCPNamesNil(t *testing.T) {
	req := adapteropenai.ChatRequest{Model: "gpt-5.3-codex"}
	got := TranslateRequest(req)
	if got.MCPToolNames != nil {
		t.Errorf("MCPToolNames = %v, want nil", got.MCPToolNames)
	}
}

func TestIsMCPToolNameHeuristic(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"CallMcpTool", true},
		{"FetchMcpResource", true},
		{"some_mcp_helper", true},
		{"MCPRunner", true},
		{"ReadFile", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isMCPToolName(tc.name); got != tc.want {
			t.Errorf("isMCPToolName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
