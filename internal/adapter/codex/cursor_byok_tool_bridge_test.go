package codex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/providers/cursor/mitmcontrib"
)

func TestCursorBYOKToolBridgeKeepsIngressCorrelationAcrossEgressAndCapture(t *testing.T) {
	const cursorCallID = "call_cursor_" + "0123456789012345678901234567890123456789012345678901234567890123456789"
	const correlationID = "cursor-byok-tool-bridge-84"
	patch := "*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch\n"
	ordinaryArguments := `{"path":"README.md"}`

	ingress := ChatRequest{Messages: []ChatMessage{
		{
			Role:    "assistant",
			Content: mustRaw(`""`),
			ToolCalls: []ToolCall{
				{ID: cursorCallID, Type: "function", Function: ToolCallFunction{Name: "ApplyPatch", Arguments: `{"input":"placeholder"}`}},
				{ID: "call_read_file", Type: "function", Function: ToolCallFunction{Name: "ReadFile", Arguments: ordinaryArguments}},
			},
		},
		{Role: "tool", ToolCallID: cursorCallID, Content: mustRaw(`"applied"`)},
		{Role: "tool", ToolCallID: "call_read_file", Content: mustRaw(`"contents"`)},
	}}
	resolved := codexResolvedForTest(ResolvedAlias{Alias: "gpt-5.6-sol", WireModel: "gpt-5.6-sol"})
	resolved.RequestID = correlationID
	resolved.Cursor.RequestID = correlationID
	egress := BuildRequestWithConfig(ingress, resolved, "", RequestBuilderConfig{})
	transport := HTTPTransportConfig{
		RequestID:       codexRequestID(*resolved),
		CursorRequestID: resolved.Cursor.RequestID,
	}
	if transport.RequestID != correlationID || transport.CursorRequestID != correlationID {
		t.Fatalf("egress correlation=%q/%q want %q", transport.RequestID, transport.CursorRequestID, correlationID)
	}
	if got := buildResponsesHTTPHeaders(transport).Get("X-Client-Request-Id"); got != correlationID {
		t.Fatalf("Codex egress correlation=%q want %q", got, correlationID)
	}

	var projectedCallID, projectedOutputID string
	for _, item := range egress.Input {
		switch item.Type {
		case "function_call":
			if item.Name == "ApplyPatch" {
				projectedCallID = item.CallID
			}
			if item.Name == "ReadFile" && item.Arguments != ordinaryArguments {
				t.Fatalf("ordinary function arguments=%q want %q", item.Arguments, ordinaryArguments)
			}
		case "function_call_output":
			if item.CallID != "call_read_file" {
				projectedOutputID = item.CallID
			}
		}
	}
	if projectedCallID == "" || projectedCallID == cursorCallID || len(projectedCallID) > codexToolCallIDMaxLength {
		t.Fatalf("projected call id=%q", projectedCallID)
	}
	if projectedOutputID != projectedCallID {
		t.Fatalf("projected output id=%q want paired call id %q", projectedOutputID, projectedCallID)
	}

	legacyInput, err := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: patch})
	if err != nil {
		t.Fatalf("marshal legacy patch input: %v", err)
	}

	for _, testCase := range []struct {
		model          string
		capturedInput  string
		capturedIsJSON bool
	}{
		{model: "clyde-codex-5.5-high", capturedInput: string(legacyInput), capturedIsJSON: true},
		{model: "gpt-5.6-sol", capturedInput: patch, capturedIsJSON: false},
	} {
		t.Run(testCase.model, func(t *testing.T) {
			captured, ok := mitmcontrib.DecodeCaptureRequest(mitmcontrib.RequestCapture{
				Path: "/aiserver.v1.AiService/BidiAppend",
				Body: cursorBidiCapturePayload(correlationID, 84, []byte(hex.EncodeToString(cursorToolEnvelope(cursorCallID, "ApplyPatch", testCase.capturedInput)))),
			})
			if !ok || len(captured.ToolEvents) != 1 {
				t.Fatalf("Bidi capture=%#v, decoded=%t", captured, ok)
			}
			event := captured.ToolEvents[0]
			if event.CallID != cursorCallID || event.Ordering != 84 || event.ToolName != "ApplyPatch" {
				t.Fatalf("captured tool event=%#v", event)
			}
			if !strings.Contains(string(captured.RepresentationJSON), correlationID) {
				t.Fatalf("capture representation omits correlation %q", correlationID)
			}
			capturedRepresentation := event.InputRepresentation
			if !testCase.capturedIsJSON {
				decoded, decodeErr := base64.StdEncoding.DecodeString(capturedRepresentation)
				if decodeErr != nil {
					t.Fatalf("decode raw captured patch: %v", decodeErr)
				}
				capturedRepresentation = string(decoded)
			}
			if capturedRepresentation != testCase.capturedInput {
				t.Fatalf("captured representation=%q want %q", capturedRepresentation, testCase.capturedInput)
			}
			testCursorPatchRepresentation(t, correlationID, testCase.model, patch, capturedRepresentation)
		})
	}
}

func testCursorPatchRepresentation(t *testing.T, correlationID string, model string, patch string, want string) {
	t.Helper()
	resolved := &adapterresolver.ResolvedRequest{}
	resolved.Cursor.NormalizedModel = model
	representation := nativePatchRepresentationForCursorRoute(resolved)
	renderer := adapterrender.NewEventRendererWithOptions(
		correlationID,
		model,
		"codex",
		nil,
		adapterrender.EventRendererOptions{NativePatchRepresentation: representation},
	)
	chunks := renderer.HandleEvent(adapterrender.ToolCallDelta{
		ToolCalls:        []adapteropenai.ToolCall{{Index: 0, Function: adapteropenai.ToolCallFunction{Name: "ApplyPatch"}}},
		NativePatchInput: &adapterrender.NativePatchInput{Input: patch, Final: true},
	})
	if len(chunks) != 1 || len(chunks[0].Choices) != 1 || len(chunks[0].Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("model %q chunks=%#v", model, chunks)
	}
	got := chunks[0].Choices[0].Delta.ToolCalls[0].Function.Arguments
	if got != want {
		t.Fatalf("model %q patch arguments=%q want %q", model, got, want)
	}
}

func cursorBidiCapturePayload(requestID string, sequence uint64, envelope []byte) []byte {
	out := cursorProtoString(nil, 1, requestID)
	out = cursorProtoVarint(out, 2, sequence)
	return cursorProtoBytes(out, 3, envelope)
}

func cursorToolEnvelope(callID string, name string, input string) []byte {
	out := cursorProtoString(nil, 1, callID)
	out = cursorProtoString(out, 2, name)
	return cursorProtoString(out, 3, input)
}

func cursorProtoString(dst []byte, field uint64, value string) []byte {
	return cursorProtoBytes(dst, field, []byte(value))
}

func cursorProtoBytes(dst []byte, field uint64, value []byte) []byte {
	dst = binary.AppendUvarint(dst, field<<3|2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func cursorProtoVarint(dst []byte, field uint64, value uint64) []byte {
	dst = binary.AppendUvarint(dst, field<<3)
	return binary.AppendUvarint(dst, value)
}
