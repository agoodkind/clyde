// Package ingresscontract declares the dependency-inversion interfaces
// the adapter ingress boundary uses to translate, identify, and
// describe an incoming OpenAI-shaped chat request without importing a
// vendor-specific ingress package. The boundary declares what it
// needs (typed primitives in, typed primitives out), and each ingress
// package supplies the concrete contract that knows how to extract
// that vendor's metadata, derive its chat identity, and emit its
// vendor-specific log attributes. Keeping the boundary blind to
// vendor ingress shapes is what enforces the layering rule: the
// generic adapter never speaks Cursor (or Continue, or Aider, or raw
// curl) ingress quirks, only primitives.
//
// This package mirrors the dependency-inversion contract style of
// internal/adapter/errcontract: a tiny, typed surface with no upstream
// dependencies. Implementations live under
// internal/adapter/<vendor>/ and register themselves into the adapter
// at process startup through a single composition-root file
// (internal/adapter/ingress_registration.go), the only file at adapter
// depth 1 that imports a vendor ingress package.
package ingresscontract

import (
	"log/slog"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/correlation"
)

// ChatRequestPrimitive is the typed wire-shape primitive an ingress
// contract receives. The adapter has already decoded the OpenAI body
// into adapteropenai.ChatRequest; the contract sees that decoded value
// plus the raw bytes for any vendor-specific metadata extraction.
//
// Body is the raw decoded body. It is read-only and never mutated by
// the ingress contract.
type ChatRequestPrimitive struct {
	Body adapteropenai.ChatRequest
}

// IngressContext is the typed primitive an ingress contract returns
// from Translate. It carries every vendor-derived field the generic
// adapter needs after the boundary, named in primitive (string, bool,
// slice) terms so the boundary never imports a vendor type.
//
// Vendor implementations may carry richer state internally; the
// boundary only reads the fields below.
type IngressContext struct {
	// User is the request user id when the vendor surfaces one.
	User string
	// RequestID is the vendor's per-request id when present.
	RequestID string
	// ConversationID is the vendor's per-conversation id when present.
	ConversationID string
	// GenerationID is the vendor's per-generation id when present.
	GenerationID string
	// WorkspacePath is the vendor's workspace identifier when present.
	WorkspacePath string
	// NormalizedModel is the vendor-normalized model alias.
	NormalizedModel string
	// PathKind names the vendor request path family
	// (foreground, background, resume, subagent, etc.). Empty when the
	// vendor does not classify request paths.
	PathKind string
	// Mode is the vendor product mode (agent, plan, etc.). Empty when
	// the vendor does not classify modes.
	Mode string
	// RawToolNames is the ordered list of tool names the request
	// declared, with vendor-specific normalization already applied.
	RawToolNames []string
	// HasSubagentTool reflects whether the vendor's spawn-subagent
	// tool was advertised on this request.
	HasSubagentTool bool
	// CanSwitchMode reflects whether the vendor advertised its mode
	// switch tool.
	CanSwitchMode bool
	// HasCreatePlanTool reflects whether the vendor advertised its
	// create-plan tool.
	HasCreatePlanTool bool
	// HasApplyPatchTool reflects whether the vendor advertised its
	// apply-patch tool.
	HasApplyPatchTool bool
	// MCPToolNames lists MCP-style tool names the vendor advertised.
	MCPToolNames []string
}

// ChatIdentityPrimitive is the typed primitive ResolveIdentity returns.
// Every field is a primitive string or int so the boundary never
// imports a vendor identity type.
type ChatIdentityPrimitive struct {
	// ChatKey is the canonical chat identifier the boundary stamps
	// onto correlation context for transcript routing.
	ChatKey string
	// ChatKeySource records how ChatKey was derived (native, derived).
	ChatKeySource string
	// ChatRootKey identifies the root chat lineage when available.
	ChatRootKey string
	// ChatBranchKey identifies the branch within ChatRootKey.
	ChatBranchKey string
	// MessageCount is the message count at identity resolution time.
	MessageCount int
	// ToolCount is the tool count at identity resolution time.
	ToolCount int
	// Fork describes a detected branch fork, if any.
	Fork ChatForkPrimitive
}

// ChatForkPrimitive describes a detected branch fork in primitive
// terms.
type ChatForkPrimitive struct {
	// Detected is true when the resolver detected a fork on this
	// request.
	Detected bool
	// PriorBranchKey names the closest existing branch before the fork.
	PriorBranchKey string
	// CommonPrefixLen records how many lineage entries the new branch
	// shares with PriorBranchKey.
	CommonPrefixLen int
}

// IngressContract is the typed boundary an adapter ingress provides.
// Implementations live under internal/adapter/<vendor>/ and translate
// an OpenAI-shaped chat request into typed primitives without leaking
// vendor types across the boundary.
type IngressContract interface {
	// Translate derives vendor-specific metadata from a decoded chat
	// request and returns the primitive-only IngressContext.
	Translate(req ChatRequestPrimitive) IngressContext

	// ResolveIdentity returns the canonical chat identity for a
	// translated request. Implementations may keep per-daemon state
	// internally; the boundary only reads ChatIdentityPrimitive.
	ResolveIdentity(corr correlation.Context, ic IngressContext, req ChatRequestPrimitive) ChatIdentityPrimitive

	// LogAttrs returns the vendor-specific structured log attributes
	// the adapter emits at boundary log sites. rawModel is the raw
	// inbound model string; toolNames is an optional override the
	// caller already computed (for example, the canonical tool name
	// list extracted at request-receive time). When toolNames is nil
	// the implementation may fall back to ic.RawToolNames.
	LogAttrs(ic IngressContext, rawModel string, toolNames []string) []slog.Attr
}

// IngressFamily names a registered ingress family at the adapter
// boundary. The string values match the adapter package's route or
// vendor identifier. The default vendor today is "cursor"; future
// vendors (Continue, Aider, raw curl) plug in by registering under
// their own family name without changing the generic adapter.
type IngressFamily string

// IngressFamilyCursor is the canonical Cursor BYOK ingress family.
const IngressFamilyCursor IngressFamily = "cursor"

// IngressRegistrar is the inversion seam the adapter ingress
// boundary exposes to vendor packages. Each vendor package's
// RegisterIngress function calls Register with its family identifier
// and the IngressContract implementation at startup. The adapter
// holds these by family and dispatches by registered family at
// runtime.
type IngressRegistrar interface {
	Register(family IngressFamily, contract IngressContract)
}
