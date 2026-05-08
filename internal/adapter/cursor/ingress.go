package cursor

import (
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	"goodkind.io/clyde/internal/correlation"
)

// Ingress is the Cursor implementation of
// ingresscontract.IngressContract. It owns every Cursor-specific
// quirk in one place: metadata extraction, mode classification,
// chat-key derivation fallback, branch fork detection, and
// vendor-specific log attributes. The generic adapter sees only
// typed primitives across this boundary.
type Ingress struct {
	resolver *ChatIdentityResolver
}

// NewIngress returns a Cursor ingress backed by a fresh per-daemon
// ChatIdentityResolver. The daemon owns one Ingress for the lifetime
// of the adapter; reload-driven reinitialization is implicit because
// the daemon constructs a fresh adapter Server on each reload.
func NewIngress() *Ingress {
	return &Ingress{
		resolver: NewChatIdentityResolver(),
	}
}

// Translate decodes a primitive ChatRequestPrimitive into a
// vendor-neutral IngressContext. All Cursor-specific knowledge
// (metadata key fallbacks, tool-presence classification, request-path
// derivation) is contained here so callers above the boundary see
// only primitive-typed fields.
func (i *Ingress) Translate(req ingresscontract.ChatRequestPrimitive) ingresscontract.IngressContext {
	cursorReq := TranslateRequest(req.Body)
	return ingressContextFromRequest(cursorReq)
}

// ResolveIdentity returns the canonical chat identity for a
// translated request as primitives. It delegates to the per-daemon
// ChatIdentityResolver, then folds the resulting ChatIdentity into
// ingresscontract.ChatIdentityPrimitive.
func (i *Ingress) ResolveIdentity(corr correlation.Context, ic ingresscontract.IngressContext, req ingresscontract.ChatRequestPrimitive) ingresscontract.ChatIdentityPrimitive {
	cursorReq := TranslateRequest(req.Body)
	overlayCursorRequestFromContext(&cursorReq, ic)
	identity := i.resolver.Resolve(corr, cursorReq, req.Body)
	return chatIdentityPrimitive(identity)
}

// LogAttrs returns the vendor-specific structured log attributes the
// adapter emits at boundary log sites. The output shape is the same
// as BoundaryLogAttrs so on-disk log consumers are unchanged.
func (i *Ingress) LogAttrs(ic ingresscontract.IngressContext, rawModel string, toolNames []string) []slog.Attr {
	normalizedModel := ic.NormalizedModel
	if normalizedModel == "" {
		normalizedModel = NormalizeModelAlias(rawModel)
	}
	mode := ic.Mode
	if mode == "" {
		mode = string(modeFromTools(ic.RawToolNames))
	}
	if len(toolNames) == 0 {
		toolNames = ic.RawToolNames
	}
	canSpawnAgent := ic.HasSubagentTool
	canSwitchMode := ic.CanSwitchMode

	attrs := []slog.Attr{
		slog.String("cursor_request_path", ic.PathKind),
		slog.String("cursor_raw_model", strings.TrimSpace(rawModel)),
		slog.String("cursor_normalized_model", normalizedModel),
		slog.Any("cursor_raw_tool_names", toolNames),
		slog.String("cursor_mode", mode),
		slog.Bool("cursor_can_spawn_agent", canSpawnAgent),
		slog.Bool("cursor_can_switch_mode", canSwitchMode),
	}
	if ic.ConversationID != "" {
		attrs = append(attrs, slog.String("cursor_conversation_id", ic.ConversationID))
	}
	if ic.RequestID != "" {
		attrs = append(attrs, slog.String("cursor_request_id", ic.RequestID))
	}
	if ic.GenerationID != "" {
		attrs = append(attrs, slog.String("cursor_generation_id", ic.GenerationID))
	}
	attrs = append(attrs, slog.Bool("cursor_generation_id_present", ic.GenerationID != ""))
	return attrs
}

// RegisterIngress is the composition-root hook the adapter calls
// from internal/adapter/ingress_registration.go to wire the Cursor
// ingress contract into the package-level registrar without forcing
// the generic adapter to import this package outside that single
// file.
func RegisterIngress(reg ingresscontract.IngressRegistrar) {
	reg.Register(ingresscontract.IngressFamilyCursor, NewIngress())
}

// modeFromTools mirrors the lightweight mode-from-toolnames logic
// requestMode uses, so LogAttrs can derive a mode label from an
// IngressContext that arrived without one.
func modeFromTools(toolNames []string) Mode {
	if hasRawToolName(toolNames, "CreatePlan") {
		return ModePlan
	}
	return ModeAgent
}

// ingressContextFromRequest folds a typed Cursor Request into the
// primitive-only IngressContext. Keeping every vendor-specific field
// behind this projection is what lets the boundary stay typed without
// importing the cursor package above the registration root.
func ingressContextFromRequest(req Request) ingresscontract.IngressContext {
	return ingresscontract.IngressContext{
		User:              req.User,
		RequestID:         req.RequestID,
		ConversationID:    req.ConversationID,
		GenerationID:      req.GenerationID,
		WorkspacePath:     req.WorkspacePath,
		NormalizedModel:   req.NormalizedModel,
		PathKind:          string(req.PathKind),
		Mode:              string(req.Mode),
		RawToolNames:      cloneStringsSlice(req.RawToolNames),
		HasSubagentTool:   req.HasSubagentTool,
		CanSwitchMode:     req.CanSwitchMode,
		HasCreatePlanTool: req.HasCreatePlanTool,
		HasApplyPatchTool: req.HasApplyPatchTool,
		MCPToolNames:      cloneStringsSlice(req.MCPToolNames),
	}
}

// overlayCursorRequestFromContext copies primitive-only fields from
// an IngressContext back onto a freshly-translated Request. The
// adapter may stamp identity fields onto the IngressContext (e.g.
// after correlation lookup); this overlay applies those primitive
// updates so the inner resolver sees the same view across the
// boundary.
func overlayCursorRequestFromContext(req *Request, ic ingresscontract.IngressContext) {
	if ic.User != "" {
		req.User = ic.User
	}
	if ic.RequestID != "" {
		req.RequestID = ic.RequestID
	}
	if ic.ConversationID != "" {
		req.ConversationID = ic.ConversationID
	}
	if ic.GenerationID != "" {
		req.GenerationID = ic.GenerationID
	}
	if ic.WorkspacePath != "" {
		req.WorkspacePath = ic.WorkspacePath
	}
	if ic.NormalizedModel != "" {
		req.NormalizedModel = ic.NormalizedModel
	}
	if ic.PathKind != "" {
		req.PathKind = RequestPathKind(ic.PathKind)
	}
	if ic.Mode != "" {
		req.Mode = Mode(ic.Mode)
	}
}

// chatIdentityPrimitive folds a typed ChatIdentity into the
// primitive-only ChatIdentityPrimitive.
func chatIdentityPrimitive(identity ChatIdentity) ingresscontract.ChatIdentityPrimitive {
	return ingresscontract.ChatIdentityPrimitive{
		ChatKey:       identity.ChatKey,
		ChatKeySource: identity.ChatKeySource,
		ChatRootKey:   identity.ChatRootKey,
		ChatBranchKey: identity.ChatBranchKey,
		MessageCount:  identity.MessageCount,
		ToolCount:     identity.ToolCount,
		Fork: ingresscontract.ChatForkPrimitive{
			Detected:        identity.Fork.Detected,
			PriorBranchKey:  identity.Fork.PriorBranchKey,
			CommonPrefixLen: identity.Fork.CommonPrefixLen,
		},
	}
}

func cloneStringsSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}
