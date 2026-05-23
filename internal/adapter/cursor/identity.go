package cursor

import (
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/logevent"
)

const (
	// HeaderRequestID is Cursor's OpenAI-compatible ingress request id.
	HeaderRequestID = "x-cursor-request-id"
	// HeaderConversationID is Cursor's OpenAI-compatible ingress chat id.
	HeaderConversationID = "x-cursor-conversation-id"
	// HeaderGenerationID is Cursor's OpenAI-compatible ingress generation id.
	HeaderGenerationID = "x-cursor-generation-id"
)

// FacetKey is the typed request-story key for Cursor ingress metadata.
const FacetKey = "cursor"

// Facet is the Cursor-owned request-story contribution attached through
// the generic logevent.Facet interface.
type Facet struct {
	RequestID      string `json:"request_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	GenerationID   string `json:"generation_id,omitempty"`
}

// FacetKey returns the Cursor facet wire key.
func (f Facet) FacetKey() string { return FacetKey }

// FacetAttrs returns the Cursor identity attrs under the Cursor facet group.
func (f Facet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 3)
	if f.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", f.RequestID))
	}
	if f.ConversationID != "" {
		attrs = append(attrs, slog.String("conversation_id", f.ConversationID))
	}
	if f.GenerationID != "" {
		attrs = append(attrs, slog.String("generation_id", f.GenerationID))
	}
	return attrs
}

// SinkHints reports that Cursor ingress identity does not add sinks.
func (f Facet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		HasRawCapture:        false,
		NeedsProviderSidecar: false,
	}
}

func (f Facet) empty() bool {
	return f.RequestID == "" &&
		f.ConversationID == "" &&
		f.GenerationID == ""
}

// TranslateHeaders extracts Cursor identity fields that are available
// before the request body is decoded.
func (i *Ingress) TranslateHeaders(header http.Header) ingresscontract.IngressContext {
	if header == nil {
		return ingresscontract.IngressContext{
			User:              "",
			RequestID:         "",
			ConversationID:    "",
			GenerationID:      "",
			WorkspacePath:     "",
			NormalizedModel:   "",
			PathKind:          "",
			Mode:              "",
			RawToolNames:      nil,
			HasSubagentTool:   false,
			CanSwitchMode:     false,
			HasCreatePlanTool: false,
			HasApplyPatchTool: false,
			MCPToolNames:      nil,
		}
	}
	return ingresscontract.IngressContext{
		User:              "",
		RequestID:         firstHeader(header, HeaderRequestID),
		ConversationID:    firstHeader(header, HeaderConversationID),
		GenerationID:      firstHeader(header, HeaderGenerationID),
		WorkspacePath:     "",
		NormalizedModel:   "",
		PathKind:          "",
		Mode:              "",
		RawToolNames:      nil,
		HasSubagentTool:   false,
		CanSwitchMode:     false,
		HasCreatePlanTool: false,
		HasApplyPatchTool: false,
		MCPToolNames:      nil,
	}
}

// CorrelationAttrs returns Cursor-owned correlation attributes for
// downstream structured logs.
func (i *Ingress) CorrelationAttrs(ic ingresscontract.IngressContext) []correlation.IdentityAttribute {
	attrs := make([]correlation.IdentityAttribute, 0, 3)
	if ic.RequestID != "" {
		attrs = append(attrs, correlation.IdentityAttribute{Key: "cursor_request_id", Value: ic.RequestID})
	}
	if ic.ConversationID != "" {
		attrs = append(attrs, correlation.IdentityAttribute{Key: "cursor_conversation_id", Value: ic.ConversationID})
	}
	if ic.GenerationID != "" {
		attrs = append(attrs, correlation.IdentityAttribute{Key: "cursor_generation_id", Value: ic.GenerationID})
	}
	return attrs
}

// RequestFacets returns the Cursor-owned request-story facet for ic.
func (i *Ingress) RequestFacets(ic ingresscontract.IngressContext) []logevent.Facet {
	facet := Facet{
		RequestID:      ic.RequestID,
		ConversationID: ic.ConversationID,
		GenerationID:   ic.GenerationID,
	}
	if facet.empty() {
		return nil
	}
	return []logevent.Facet{facet}
}

func firstHeader(header http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}
