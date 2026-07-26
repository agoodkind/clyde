package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/codexwire"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// cursorNativeGPTRoute is a Cursor model route that is not a
// clyde-codex-* route, so it takes the raw native patch contract.
const cursorNativeGPTRoute = "gpt-5.6-sol"

// cursorApplyPatchPatch is a well-formed patch body in codex's freeform
// apply_patch envelope.
const cursorApplyPatchPatch = "*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch\n"

// cursorChatRequestJSON is the tools-and-messages part of a real Cursor
// BYOK /v1/chat/completions body: ApplyPatch declared as an OpenAI
// custom (freeform) tool with a lark grammar, alongside an ordinary
// function tool. Decoding the raw bytes exercises the same ingress path
// a live request takes.
const cursorChatRequestJSON = `{
  "model": "gpt-5.6-sol",
  "messages": [{"role": "user", "content": "create out.md containing ok"}],
  "tools": [
    {"type": "function", "function": {"name": "ReadFile", "description": "Read a file.",
      "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}},
    {"type": "custom", "name": "ApplyPatch", "description": "Use this tool to edit files.",
      "format": {"type": "grammar", "syntax": "lark",
        "definition": "start: begin_patch hunk end_patch\nbegin_patch: \"*** Begin Patch\" LF\nend_patch: \"*** End Patch\" LF?\n"}}
  ]
}`

// upstreamRequestBody is the decoded view of the bytes clyde actually
// POSTs to the codex Responses endpoint. Tests assert against the
// serialized body rather than the in-memory struct so a field lost at
// marshal time cannot pass.
type upstreamRequestBody struct {
	Model string                `json:"model"`
	Input []codexwire.InputItem `json:"input"`
	Tools []codexwire.ToolSpec  `json:"tools"`
}

func marshalUpstreamBody(t *testing.T, out HTTPTransportRequest) upstreamRequestBody {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal upstream request: %v", err)
	}
	var body upstreamRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	return body
}

func cursorNativeGPTResolved() *adapterresolver.ResolvedRequest {
	resolved := codexResolvedForTest(ResolvedAlias{Alias: cursorNativeGPTRoute, WireModel: cursorNativeGPTRoute})
	resolved.Cursor.NormalizedModel = cursorNativeGPTRoute
	return resolved
}

func decodeCursorChatRequest(t *testing.T) adapteropenai.ChatRequest {
	t.Helper()
	var req adapteropenai.ChatRequest
	if err := json.Unmarshal([]byte(cursorChatRequestJSON), &req); err != nil {
		t.Fatalf("decode Cursor chat request: %v", err)
	}
	return req
}

func findToolSpec(t *testing.T, tools []codexwire.ToolSpec, name string) codexwire.ToolSpec {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q missing from upstream tools: %+v", name, tools)
	return codexwire.ToolSpec{}
}

// TestCursorCustomToolReachesUpstreamAsFreeformTool proves the outbound
// leg: a freeform ApplyPatch declaration must arrive at the codex
// Responses endpoint as a custom tool carrying its grammar. Forwarding
// it as a function tool with no parameters is what left the model no
// place to put the patch, so it answered with empty arguments.
func TestCursorCustomToolReachesUpstreamAsFreeformTool(t *testing.T) {
	req := decodeCursorChatRequest(t)

	body := marshalUpstreamBody(t, BuildRequestWithConfig(req, cursorNativeGPTResolved(), "", RequestBuilderConfig{}))

	patchTool := findToolSpec(t, body.Tools, "ApplyPatch")
	if patchTool.Type != codexwire.ToolSpecTypeCustom {
		t.Fatalf("ApplyPatch type = %q, want %q", patchTool.Type, codexwire.ToolSpecTypeCustom)
	}
	if patchTool.Format == nil {
		t.Fatalf("ApplyPatch reached the upstream with no format: %+v", patchTool)
	}
	if patchTool.Format.Type != codexwire.ToolFormatTypeGrammar || patchTool.Format.Syntax != "lark" {
		t.Fatalf("ApplyPatch format = %+v, want a lark grammar", patchTool.Format)
	}
	if !strings.Contains(patchTool.Format.Definition, "*** Begin Patch") {
		t.Fatalf("ApplyPatch grammar lost its patch envelope: %q", patchTool.Format.Definition)
	}
	if len(patchTool.Parameters) != 0 {
		t.Fatalf("ApplyPatch carried parameters %q, want none on a freeform tool", patchTool.Parameters)
	}

	readTool := findToolSpec(t, body.Tools, "ReadFile")
	if readTool.Type != codexwire.ToolSpecTypeFunction {
		t.Fatalf("ReadFile type = %q, want %q", readTool.Type, codexwire.ToolSpecTypeFunction)
	}
	if !strings.Contains(string(readTool.Parameters), `"path"`) {
		t.Fatalf("ReadFile lost its parameter schema: %q", readTool.Parameters)
	}
	if readTool.Format != nil {
		t.Fatalf("ReadFile gained a format: %+v", readTool.Format)
	}
}

// customToolCallStream is the SSE reply the codex Responses endpoint
// sends for a freeform tool call, matching a live capture: an added
// custom_tool_call item, streamed input deltas, then the completed item.
func customToolCallStream(toolName, patch string) *strings.Reader {
	head, tail := patch[:len("*** Begin Patch\n")], patch[len("*** Begin Patch\n"):]
	return strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"` + toolName + `","input":""}}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":` + jsonString(head) + `}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":` + jsonString(tail) + `}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"` + toolName + `","input":` + jsonString(patch) + `}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
}

func jsonString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// renderCursorToolCalls runs the real codex SSE parser and the real
// event renderer for the native-GPT Cursor route, returning the tool
// calls Cursor would receive on the wire.
func renderCursorToolCalls(t *testing.T, stream *strings.Reader, declared []codexwire.ToolSpec) []adapteropenai.ToolCall {
	t.Helper()
	representation := nativePatchRepresentationForCursorRoute(cursorNativeGPTResolved())
	renderer := adapterrender.NewEventRendererWithOptions(
		"req-custom-tool",
		cursorNativeGPTRoute,
		"codex",
		nil,
		adapterrender.EventRendererOptions{NativePatchRepresentation: representation},
	)
	var chunks []adapteropenai.StreamChunk
	_, err := ParseSSEEventsWithOptions(context.Background(), stream, func(ev adapterrender.Event) error {
		chunks = append(chunks, renderer.HandleEvent(ev)...)
		return nil
	}, sseInstrumentationContext{}, SSEParseOptions{
		DeclaredTools:             declared,
		NativePatchRepresentation: representation,
	})
	if err != nil {
		t.Fatalf("parse codex SSE: %v", err)
	}
	return collectToolCallsLocal(chunks)
}

// TestCursorCustomToolCallReachesCursorAsRawPatch proves the return
// leg: the freeform reply must reach Cursor as raw patch bytes under
// the client's declared tool name, because Cursor feeds a tool call's
// payload straight into its patch parser.
func TestCursorCustomToolCallReachesCursorAsRawPatch(t *testing.T) {
	req := decodeCursorChatRequest(t)
	declared := BuildRequestWithConfig(req, cursorNativeGPTResolved(), "", RequestBuilderConfig{}).Tools

	calls := renderCursorToolCalls(t, customToolCallStream("ApplyPatch", cursorApplyPatchPatch), declared)

	if len(calls) == 0 {
		t.Fatalf("no tool calls reached the client")
	}
	if calls[0].Function.Name != "ApplyPatch" {
		t.Fatalf("tool name = %q, want ApplyPatch", calls[0].Function.Name)
	}
	var payload strings.Builder
	for _, call := range calls {
		payload.WriteString(call.Function.Arguments)
	}
	if payload.String() != cursorApplyPatchPatch {
		t.Fatalf("client payload = %q, want the raw patch %q", payload.String(), cursorApplyPatchPatch)
	}
}

// TestBackendInjectedApplyPatchMapsToDeclaredCustomTool covers the case
// where the codex backend answers with its own apply_patch tool type
// rather than the client's name. The declared custom tool carries no
// parameter schema, so the parameter-shape classifier cannot match it;
// the freeform declaration itself is the signal.
func TestBackendInjectedApplyPatchMapsToDeclaredCustomTool(t *testing.T) {
	req := decodeCursorChatRequest(t)
	declared := BuildRequestWithConfig(req, cursorNativeGPTResolved(), "", RequestBuilderConfig{}).Tools

	calls := renderCursorToolCalls(t, customToolCallStream("apply_patch", cursorApplyPatchPatch), declared)

	if len(calls) == 0 {
		t.Fatalf("no tool calls reached the client")
	}
	if calls[0].Function.Name != "ApplyPatch" {
		t.Fatalf("tool name = %q, want the client-declared ApplyPatch", calls[0].Function.Name)
	}
}

// TestCursorCustomToolReplayKeepsFreeformShape proves the follow-up
// turn. Cursor echoes a prior custom call back inside tool_calls with
// type "function" and the raw patch as arguments. Replaying that as a
// function_call would hand the upstream arguments that are not JSON, so
// the declared tool set has to decide the shape.
func TestCursorCustomToolReplayKeepsFreeformShape(t *testing.T) {
	req := decodeCursorChatRequest(t)
	req.Messages = append(req.Messages,
		adapteropenai.ChatMessage{
			Role: "assistant",
			ToolCalls: []adapteropenai.ToolCall{{
				Index:    0,
				ID:       "call_patch",
				Type:     "function",
				Function: adapteropenai.ToolCallFunction{Name: "ApplyPatch", Arguments: cursorApplyPatchPatch},
			}},
		},
		adapteropenai.ChatMessage{
			Role:       "tool",
			ToolCallID: "call_patch",
			Content:    mustRaw(`"Applied patch to out.md"`),
		},
	)

	body := marshalUpstreamBody(t, BuildRequestWithConfig(req, cursorNativeGPTResolved(), "", RequestBuilderConfig{}))

	var call, output *codexwire.InputItem
	for i := range body.Input {
		switch body.Input[i].Type {
		case codexwire.ItemTypeCustomToolCall:
			call = &body.Input[i]
		case codexwire.ItemTypeCustomToolCallOutput:
			output = &body.Input[i]
		case codexwire.ItemTypeFunctionCall, codexwire.ItemTypeFunctionCallOutput:
			t.Fatalf("ApplyPatch replayed under the function shape: %+v", body.Input[i])
		case codexwire.ItemTypeMessage, codexwire.ItemTypeLocalShellCall, codexwire.ItemTypeReasoning:
		}
	}
	if call == nil {
		t.Fatalf("no custom_tool_call in the replayed input: %+v", body.Input)
	}
	if call.Name != "ApplyPatch" {
		t.Fatalf("replayed call name = %q, want ApplyPatch", call.Name)
	}
	if call.Input != cursorApplyPatchPatch {
		t.Fatalf("replayed call input = %q, want the raw patch", call.Input)
	}
	if call.Arguments != "" {
		t.Fatalf("replayed call carried JSON arguments %q, want none", call.Arguments)
	}
	if output == nil {
		t.Fatalf("no custom_tool_call_output in the replayed input: %+v", body.Input)
	}
	if output.Name != "ApplyPatch" {
		t.Fatalf("replayed output name = %q, want ApplyPatch", output.Name)
	}
	if !strings.Contains(string(output.Output), "Applied patch") {
		t.Fatalf("replayed output body = %q", output.Output)
	}
}

// TestFunctionToolReplayKeepsFunctionShape guards the ordinary path:
// a tool the client declared as a function still replays as a
// function_call with its JSON arguments intact.
func TestFunctionToolReplayKeepsFunctionShape(t *testing.T) {
	req := decodeCursorChatRequest(t)
	req.Messages = append(req.Messages,
		adapteropenai.ChatMessage{
			Role: "assistant",
			ToolCalls: []adapteropenai.ToolCall{{
				Index:    0,
				ID:       "call_read",
				Type:     "function",
				Function: adapteropenai.ToolCallFunction{Name: "ReadFile", Arguments: `{"path":"out.md"}`},
			}},
		},
		adapteropenai.ChatMessage{
			Role:       "tool",
			ToolCallID: "call_read",
			Content:    mustRaw(`"ok"`),
		},
	)

	body := marshalUpstreamBody(t, BuildRequestWithConfig(req, cursorNativeGPTResolved(), "", RequestBuilderConfig{}))

	var call *codexwire.InputItem
	for i := range body.Input {
		if body.Input[i].Type == codexwire.ItemTypeFunctionCall {
			call = &body.Input[i]
		}
		if body.Input[i].Type == codexwire.ItemTypeCustomToolCall {
			t.Fatalf("ReadFile replayed under the freeform shape: %+v", body.Input[i])
		}
	}
	if call == nil {
		t.Fatalf("no function_call in the replayed input: %+v", body.Input)
	}
	if call.Name != "ReadFile" || call.Arguments != `{"path":"out.md"}` {
		t.Fatalf("replayed function call = %+v", call)
	}
}
