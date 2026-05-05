package codex

import (
	"context"
	"fmt"
	"strings"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

// ReasoningEffort is the provider-level enum accepted by Codex live turns.
type ReasoningEffort = adaptercodex.ReasoningEffort

// LiveRuntime is Clyde's provider-private contract for driving a live Codex
// conversation without exposing Codex transport details to daemon, UI, or
// generic session code.
type LiveRuntime interface {
	Start(context.Context, LiveStartRequest) (*LiveSession, error)
	Attach(context.Context, LiveAttachRequest) (*LiveSession, error)
	Send(context.Context, LiveSendRequest) (*LiveTurn, error)
	Stream(context.Context, LiveStreamRequest) (<-chan LiveEvent, error)
	Stop(context.Context, LiveStopRequest) error
	Close() error
}

// LiveStartRequest describes a new Codex live runtime thread.
type LiveStartRequest struct {
	WorkDir               string
	Model                 string
	ModelProvider         string
	DeveloperInstructions string
	BaseInstructions      string
	SessionName           string
	Ephemeral             bool
}

// LiveAttachRequest identifies an existing Codex live runtime thread.
type LiveAttachRequest struct {
	ThreadID string
}

// LiveSendRequest describes one user turn sent to a Codex live runtime.
type LiveSendRequest struct {
	ThreadID string
	Text     string
	WorkDir  string
	Model    string
	Effort   ReasoningEffort
}

// LiveStreamRequest identifies one Codex turn stream.
type LiveStreamRequest struct {
	ThreadID string
	TurnID   string
}

// LiveStopRequest identifies one Codex turn to interrupt.
type LiveStopRequest struct {
	ThreadID string
	TurnID   string
}

// LiveSession is the provider-level Codex live session handle.
type LiveSession struct {
	ThreadID string
	WorkDir  string
	Model    string
}

// LiveTurn is the provider-level result of starting one Codex turn.
type LiveTurn struct {
	ThreadID string
	TurnID   string
	Status   LiveTurnStatus
}

// LiveTurnStatus is the Codex live turn lifecycle state.
type LiveTurnStatus string

const (
	// LiveTurnStatusCompleted means Codex completed the turn.
	LiveTurnStatusCompleted LiveTurnStatus = "completed"
	// LiveTurnStatusInterrupted means Codex interrupted the turn.
	LiveTurnStatusInterrupted LiveTurnStatus = "interrupted"
	// LiveTurnStatusFailed means Codex failed the turn.
	LiveTurnStatusFailed LiveTurnStatus = "failed"
	// LiveTurnStatusInProgress means Codex is still running the turn.
	LiveTurnStatusInProgress LiveTurnStatus = "inProgress"
)

// LiveEvent is one provider-normalized Codex live stream event.
type LiveEvent struct {
	Kind     LiveEventKind
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string
	Status   LiveTurnStatus
	Err      error
}

// LiveEventKind is the provider-normalized live stream event kind.
type LiveEventKind string

const (
	// LiveEventDelta carries assistant text.
	LiveEventDelta LiveEventKind = "delta"
	// LiveEventCompleted carries terminal turn status.
	LiveEventCompleted LiveEventKind = "completed"
	// LiveEventError carries a stream error.
	LiveEventError LiveEventKind = "error"
)

// LiveRuntimeOptions configures the Codex app-server runtime.
type LiveRuntimeOptions struct {
	CodexBin       string
	Command        []string
	WorkDir        string
	Env            []string
	ClientName     string
	ClientTitle    string
	ClientVersion  string
	Experimental   bool
	ConfigOverride []string
}

// NewLiveRuntime creates the default Codex live runtime implementation.
func NewLiveRuntime(opts LiveRuntimeOptions) LiveRuntime {
	return NewAppServerRuntime(AppServerRuntimeOptions(opts))
}

func validateLiveStartRequest(req LiveStartRequest) error {
	if strings.TrimSpace(req.WorkDir) == "" {
		return fmt.Errorf("missing codex live work dir")
	}
	return nil
}

func validateLiveAttachRequest(req LiveAttachRequest) error {
	if strings.TrimSpace(req.ThreadID) == "" {
		return fmt.Errorf("missing codex live thread id")
	}
	return nil
}

func validateLiveSendRequest(req LiveSendRequest) error {
	if strings.TrimSpace(req.ThreadID) == "" {
		return fmt.Errorf("missing codex live thread id")
	}
	if strings.TrimSpace(req.Text) == "" {
		return fmt.Errorf("missing codex live input text")
	}
	if !validReasoningEffort(req.Effort) {
		return fmt.Errorf("unsupported codex live effort %q", req.Effort)
	}
	return nil
}

func validateLiveStreamRequest(req LiveStreamRequest) error {
	if strings.TrimSpace(req.ThreadID) == "" {
		return fmt.Errorf("missing codex live thread id")
	}
	if strings.TrimSpace(req.TurnID) == "" {
		return fmt.Errorf("missing codex live turn id")
	}
	return nil
}

func validateLiveStopRequest(req LiveStopRequest) error {
	if strings.TrimSpace(req.ThreadID) == "" {
		return fmt.Errorf("missing codex live thread id")
	}
	if strings.TrimSpace(req.TurnID) == "" {
		return fmt.Errorf("missing codex live turn id")
	}
	return nil
}

// ParseReasoningEffort narrows the generic live-session effort string to the
// Codex live-turn effort set Clyde supports.
func ParseReasoningEffort(raw string) (ReasoningEffort, error) {
	effort := ReasoningEffort(strings.TrimSpace(raw))
	if validReasoningEffort(effort) {
		return effort, nil
	}
	return adaptercodex.ReasoningEffortUnset, fmt.Errorf("unsupported codex live effort %q", raw)
}

func validReasoningEffort(effort ReasoningEffort) bool {
	switch effort {
	case adaptercodex.ReasoningEffortUnset,
		adaptercodex.ReasoningEffortLow,
		adaptercodex.ReasoningEffortMedium,
		adaptercodex.ReasoningEffortHigh,
		adaptercodex.ReasoningEffortXHigh:
		return true
	default:
		return false
	}
}
