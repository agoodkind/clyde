package cursor

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/gklog/correlation"
)

func newTestIngress() *Ingress {
	return NewIngress()
}

func newCursorChatRequest(model string, conversationID string, generationID string) adapteropenai.ChatRequest {
	metadata := map[string]string{}
	if conversationID != "" {
		metadata["cursorConversationId"] = conversationID
		metadata["cursorRequestId"] = conversationID + "-req"
	}
	if generationID != "" {
		metadata["cursorGenerationId"] = generationID
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return adapteropenai.ChatRequest{
		Model:    model,
		Metadata: raw,
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hello world"`)},
		},
	}
}

func TestIngressTranslateExposesCursorPrimitives(t *testing.T) {
	t.Parallel()
	ingress := newTestIngress()
	body := newCursorChatRequest("clyde-opus-4-7", "conv-123", "gen-9")
	ic := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: body})
	if ic.ConversationID != "conv-123" {
		t.Fatalf("ConversationID = %q want conv-123", ic.ConversationID)
	}
	if ic.GenerationID != "gen-9" {
		t.Fatalf("GenerationID = %q want gen-9", ic.GenerationID)
	}
	if ic.NormalizedModel != "clyde-opus-4-7" {
		t.Fatalf("NormalizedModel = %q want clyde-opus-4-7", ic.NormalizedModel)
	}
	if ic.PathKind != string(RequestPathForeground) {
		t.Fatalf("PathKind = %q want %q", ic.PathKind, RequestPathForeground)
	}
}

func TestIngressResolveIdentityReturnsNativeWhenConversationIDPresent(t *testing.T) {
	t.Parallel()
	ingress := newTestIngress()
	body := newCursorChatRequest("clyde-opus-4-7", "conv-native", "")
	ic := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: body})
	identity := ingress.ResolveIdentity(correlation.Context{}, ic, ingresscontract.ChatRequestPrimitive{Body: body})
	if identity.ChatKey != "conv-native" {
		t.Fatalf("ChatKey = %q want conv-native", identity.ChatKey)
	}
	if identity.ChatKeySource != ChatKeySourceNative {
		t.Fatalf("ChatKeySource = %q want %q", identity.ChatKeySource, ChatKeySourceNative)
	}
}

func TestIngressResolveIdentityDerivesWhenNoConversationID(t *testing.T) {
	t.Parallel()
	ingress := newTestIngress()
	body := newCursorChatRequest("clyde-opus-4-7", "", "")
	ic := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: body})
	identity := ingress.ResolveIdentity(correlation.Context{}, ic, ingresscontract.ChatRequestPrimitive{Body: body})
	if identity.ChatKeySource != ChatKeySourceDerived {
		t.Fatalf("ChatKeySource = %q want %q", identity.ChatKeySource, ChatKeySourceDerived)
	}
	if !strings.Contains(identity.ChatKey, ".b") {
		t.Fatalf("derived ChatKey should carry branch suffix, got %q", identity.ChatKey)
	}
	if identity.ChatRootKey == "" {
		t.Fatalf("ChatRootKey empty for derived identity")
	}
}

func TestIngressLogAttrsEmitsCursorAttrs(t *testing.T) {
	t.Parallel()
	ingress := newTestIngress()
	body := newCursorChatRequest("clyde-opus-4-7", "conv-1", "gen-1")
	body.Tools = []adapteropenai.Tool{
		{Type: "function", Function: adapteropenai.ToolFunctionSchema{Name: "ApplyPatch"}},
	}
	ic := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: body})
	attrs := ingress.LogAttrs(ic, body.Model, []string{"ApplyPatch"})
	if len(attrs) == 0 {
		t.Fatalf("LogAttrs returned no attrs")
	}
	keys := map[string]bool{}
	for _, a := range attrs {
		keys[a.Key] = true
	}
	required := []string{
		"cursor_request_path",
		"cursor_raw_model",
		"cursor_normalized_model",
		"cursor_raw_tool_names",
		"cursor_mode",
		"cursor_can_spawn_agent",
		"cursor_can_switch_mode",
		"cursor_conversation_id",
		"cursor_request_id",
		"cursor_generation_id",
		"cursor_generation_id_present",
	}
	for _, key := range required {
		if !keys[key] {
			t.Errorf("LogAttrs missing %q", key)
		}
	}
}

func TestRegisterIngressInstallsCursorContract(t *testing.T) {
	t.Parallel()
	registrar := &fakeIngressRegistrar{}
	RegisterIngress(registrar)
	if registrar.family != ingresscontract.IngressFamilyCursor {
		t.Fatalf("family = %q want %q", registrar.family, ingresscontract.IngressFamilyCursor)
	}
	if registrar.contract == nil {
		t.Fatalf("contract not registered")
	}
}

type fakeIngressRegistrar struct {
	family   ingresscontract.IngressFamily
	contract ingresscontract.IngressContract
}

func (f *fakeIngressRegistrar) Register(family ingresscontract.IngressFamily, contract ingresscontract.IngressContract) {
	f.family = family
	f.contract = contract
}
