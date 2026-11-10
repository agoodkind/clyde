package clispec

import (
	"context"
	"fmt"
	"strings"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

// exampleRequestID is a placeholder in the shape a provider request id takes. It
// is deliberately not a real id from any store.
const exampleRequestID = "00000000-0000-4000-8000-000000000000"

type resolveRequestInput struct {
	RequestID string
	FullScan  bool
}

func (resolveRequestInput) isClispecInput() {}

type resolveRequestPayload struct {
	RequestID string
	FullScan  bool
}

func (resolveRequestPayload) isClispecPrepared() {}

// resolveRequestOp maps a provider request id, the only identifier a Cursor chat
// exposes through its interface, to the conversation that issued it. Every other
// conversation operation accepts the same request id as a selector, so this
// operation exists for the part they cannot carry: which path answered, and the
// opt-in exhaustive scan.
func resolveRequestOp() Operation[resolveRequestInput, resolveRequestPayload] {
	return Operation[resolveRequestInput, resolveRequestPayload]{
		Name:       Name{Canonical: "conversation_resolve_request", CLIOverride: "resolve-request"},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "Resolve a provider request id to the conversation that issued it.",
		Long: "Map one provider request id to its conversation and report which path answered. " +
			"Clyde's own index answers when the id belongs to a conversation's most recent turn; otherwise Clyde runs a bounded, indexed lookup against the provider's live store. " +
			"An id neither path resolves reports not found with the reason, never a nearby conversation. " +
			"The same request id also works as the conversation selector on every other conversation operation, so reading the transcript is `clyde conversation search REQUEST_ID`.",
		Examples: []string{
			"clyde conversation resolve-request " + exampleRequestID,
			"clyde conversation resolve-request " + exampleRequestID + " --full-scan",
		},
		Args: []Arg[resolveRequestInput]{
			PositionalArg("request_id", "Provider request id, such as the value Cursor's Copy Request ID action yields.",
				func(in *resolveRequestInput, v string) { in.RequestID = v }),
		},
		Params: []Param[resolveRequestInput]{
			BoolParam("full_scan", "Read every stored turn when the bounded paths miss. This takes tens of seconds, so it never runs unless you pass it.", false,
				func(in *resolveRequestInput, v bool) { in.FullScan = v }),
		},
		New:            func() resolveRequestInput { return resolveRequestInput{RequestID: "", FullScan: false} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare: func(in resolveRequestInput) (resolveRequestPayload, error) {
			requestID := strings.TrimSpace(in.RequestID)
			if requestID == "" {
				return resolveRequestPayload{}, fmt.Errorf("request_id is required")
			}
			return resolveRequestPayload{RequestID: requestID, FullScan: in.FullScan}, nil
		},
		Run: nil,
		runResult: func(ctx context.Context, p resolveRequestPayload) (Result, error) {
			resolution, err := daemon.ResolveConversationRequest(ctx, p.RequestID, p.FullScan)
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "resolve_request_failed", "resolve conversation request", err)
			}
			return valueResult{
				Payload: resolveRequestOutputFromDomain(resolution),
				Text:    formatRequestResolution(resolution),
			}, nil
		},
	}
}

type resolveRequestOutput struct {
	RequestID      string       `json:"request_id"`
	Found          bool         `json:"found"`
	Origin         string       `json:"origin"`
	NotFoundReason string       `json:"not_found_reason,omitempty"`
	Conversation   *conv.Record `json:"conversation,omitempty"`
}

func (resolveRequestOutput) isClispecStructuredPayload() {}

func resolveRequestOutputFromDomain(resolution conv.RequestResolution) resolveRequestOutput {
	output := resolveRequestOutput{
		RequestID:      resolution.RequestID,
		Found:          resolution.Found,
		Origin:         resolution.Origin.String(),
		NotFoundReason: "",
		Conversation:   nil,
	}
	if resolution.Found {
		record := resolution.Record
		output.Conversation = &record
		return output
	}
	output.NotFoundReason = resolution.Reason.String()
	return output
}

func formatRequestResolution(resolution conv.RequestResolution) string {
	var out strings.Builder
	fmt.Fprintf(&out, "request_id: %s\n", resolution.RequestID)
	fmt.Fprintf(&out, "found: %t\n", resolution.Found)
	if !resolution.Found {
		fmt.Fprintf(&out, "not_found_reason: %s\n", resolution.Reason.String())
		fmt.Fprintf(&out, "\n%s\n", resolution.Reason.Describe())
		return out.String()
	}
	record := resolution.Record
	fmt.Fprintf(&out, "origin: %s\n", resolution.Origin.String())
	fmt.Fprintf(&out, "conversation_id: %s\n", record.ID)
	fmt.Fprintf(&out, "provider: %s\n", record.Provider.String())
	fmt.Fprintf(&out, "native_id: %s\n", record.NativeID)
	fmt.Fprintf(&out, "title: %s\n", record.Title)
	fmt.Fprintf(&out, "workspace_root: %s\n", shortPath(record.WorkspaceRoot))
	fmt.Fprintf(&out, "artifact_path: %s\n", record.ArtifactPath)
	fmt.Fprintf(&out, "updated_at: %s\n", formatTime(record.UpdatedAt))
	fmt.Fprintf(&out, "\nRead the transcript with: clyde conversation search %s\n", record.ID)
	return out.String()
}
