package codex

// ReasoningEffort mirrors codex_protocol::openai_models::ReasoningEffort
// at research/codex/codex-rs/protocol/src/openai_models.rs:43. The
// lowercase string values match the OpenAI Responses API.
type ReasoningEffort string

const (
	// ReasoningEffortUnset leaves Codex reasoning effort unspecified.
	ReasoningEffortUnset ReasoningEffort = ""
	// ReasoningEffortNone mirrors the upstream Codex none effort.
	ReasoningEffortNone ReasoningEffort = "none"
	// ReasoningEffortMinimal mirrors the upstream Codex minimal effort.
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	// ReasoningEffortLow selects low reasoning effort.
	ReasoningEffortLow ReasoningEffort = "low"
	// ReasoningEffortMedium selects medium reasoning effort.
	ReasoningEffortMedium ReasoningEffort = "medium"
	// ReasoningEffortHigh selects high reasoning effort.
	ReasoningEffortHigh ReasoningEffort = "high"
	// ReasoningEffortXHigh selects extra-high reasoning effort.
	ReasoningEffortXHigh ReasoningEffort = "xhigh"
)

// ReasoningSummary mirrors codex_protocol::config_types::ReasoningSummary
// at research/codex/codex-rs/protocol/src/config_types.rs:27.
type ReasoningSummary string

const (
	// ReasoningSummaryUnset leaves Codex reasoning summary unspecified.
	ReasoningSummaryUnset ReasoningSummary = ""
	// ReasoningSummaryAuto lets Codex choose the reasoning summary detail.
	ReasoningSummaryAuto ReasoningSummary = "auto"
	// ReasoningSummaryConcise requests a concise reasoning summary.
	ReasoningSummaryConcise ReasoningSummary = "concise"
	// ReasoningSummaryDetailed requests a detailed reasoning summary.
	ReasoningSummaryDetailed ReasoningSummary = "detailed"
	// ReasoningSummaryNone disables reasoning summaries.
	ReasoningSummaryNone ReasoningSummary = "none"
)

// AskForApproval mirrors the v2 enum at
// research/codex/codex-rs/app-server-protocol/src/protocol/v2.rs:258.
// The experimental Granular variant is intentionally omitted here
// because Clyde does not opt into experimentalApi capabilities.
type AskForApproval string

const (
	// AskForApprovalUnset leaves Codex approval policy unspecified.
	AskForApprovalUnset AskForApproval = ""
	// AskForApprovalUnlessTrusted requests approval for untrusted actions.
	AskForApprovalUnlessTrusted AskForApproval = "untrusted"
	// AskForApprovalOnFailure requests approval after command failure.
	AskForApprovalOnFailure AskForApproval = "on_failure"
	// AskForApprovalOnRequest requests approval when the model asks.
	AskForApprovalOnRequest AskForApproval = "on_request"
	// AskForApprovalNever disables approval prompts.
	AskForApprovalNever AskForApproval = "never"
)

// RPCClientInfo mirrors v1::ClientInfo at
// research/codex/codex-rs/app-server-protocol/src/protocol/v1.rs:36.
type RPCClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// RPCInitializeCapabilities mirrors v1::InitializeCapabilities at
// research/codex/codex-rs/app-server-protocol/src/protocol/v1.rs:45.
type RPCInitializeCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi,omitempty"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
}

// RPCInitializeParams mirrors v1::InitializeParams at
// research/codex/codex-rs/app-server-protocol/src/protocol/v1.rs:28.
type RPCInitializeParams struct {
	ClientInfo   RPCClientInfo              `json:"clientInfo"`
	Capabilities *RPCInitializeCapabilities `json:"capabilities,omitempty"`
}

// RPCThreadStartParams mirrors the v2::ThreadStartParams subset Clyde
// actually emits today. Source:
// research/codex/codex-rs/app-server-protocol/src/protocol/v2.rs:3343.
type RPCThreadStartParams struct {
	Model                  string         `json:"model,omitempty"`
	Cwd                    string         `json:"cwd,omitempty"`
	ApprovalPolicy         AskForApproval `json:"approvalPolicy,omitempty"`
	BaseInstructions       string         `json:"baseInstructions,omitempty"`
	DeveloperInstructions  string         `json:"developerInstructions,omitempty"`
	Ephemeral              bool           `json:"ephemeral,omitempty"`
	ServiceName            string         `json:"serviceName,omitempty"`
	ExperimentalRawEvents  bool           `json:"experimentalRawEvents"`
	PersistExtendedHistory bool           `json:"persistExtendedHistory"`
}

// RPCThreadStartResponse mirrors the v2 thread/start response subset Clyde
// reads from the Codex app server.
type RPCThreadStartResponse struct {
	Thread RPCThread `json:"thread"`
	Model  string    `json:"model,omitempty"`
}

// RPCThread mirrors the thread identity object returned by Codex.
type RPCThread struct {
	ID string `json:"id"`
}

// RPCByteRange mirrors Codex byte ranges for text elements.
type RPCByteRange struct {
	Start        int `json:"start"`
	EndExclusive int `json:"endExclusive"`
}

// RPCTextElement mirrors the text element metadata on user input items.
type RPCTextElement struct {
	ByteRange   RPCByteRange `json:"byteRange"`
	Placeholder *string      `json:"placeholder"`
}

// RPCTurnInputItem mirrors the Text variant of v2::UserInput at
// research/codex/codex-rs/app-server-protocol/src/protocol/v2.rs:5411.
// Clyde's app-fallback path only emits text input items today; image,
// local-image, skill, and mention variants are not used.
type RPCTurnInputItem struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	TextElements []RPCTextElement `json:"text_elements"`
}

// RPCTurnStartParams mirrors the v2::TurnStartParams subset Clyde
// emits. Source:
// research/codex/codex-rs/app-server-protocol/src/protocol/v2.rs:5158.
//
// Both `effort` and `summary` are canonical turn-scoped overrides per
// the Rust schema; they are not Clyde extensions.
type RPCTurnStartParams struct {
	ThreadID       string             `json:"threadId"`
	Input          []RPCTurnInputItem `json:"input"`
	CWD            string             `json:"cwd,omitempty"`
	ApprovalPolicy AskForApproval     `json:"approvalPolicy,omitempty"`
	Model          string             `json:"model,omitempty"`
	Effort         ReasoningEffort    `json:"effort,omitempty"`
	Summary        ReasoningSummary   `json:"summary,omitempty"`
}

// RPCThreadArchiveParams mirrors v2::ThreadArchiveParams at
// research/codex/codex-rs/app-server-protocol/src/protocol/v2.rs:3659.
type RPCThreadArchiveParams struct {
	ThreadID string `json:"threadId"`
}

// RPCAgentMessageDeltaNotification mirrors assistant text deltas.
type RPCAgentMessageDeltaNotification struct {
	Delta string `json:"delta"`
}

// RPCTurnPlanStep mirrors one Codex plan step.
type RPCTurnPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// RPCTurnPlanUpdatedNotification mirrors Codex plan update notifications.
type RPCTurnPlanUpdatedNotification struct {
	Explanation string            `json:"explanation"`
	Plan        []RPCTurnPlanStep `json:"plan"`
}

// RPCItemNotification mirrors Codex item notifications scoped to a thread or
// turn.
type RPCItemNotification struct {
	Item     RPCThreadItem `json:"item"`
	ThreadID string        `json:"threadId,omitempty"`
	TurnID   string        `json:"turnId,omitempty"`
}

// RPCThreadItem mirrors the subset of Codex thread items Clyde reads.
type RPCThreadItem struct {
	Type    string                `json:"type"`
	ID      string                `json:"id"`
	Status  string                `json:"status,omitempty"`
	Command string                `json:"command,omitempty"`
	Server  string                `json:"server,omitempty"`
	Tool    string                `json:"tool,omitempty"`
	Changes []RPCFileUpdateChange `json:"changes,omitempty"`
}

// RPCFileUpdateChange mirrors file update changes carried by Codex items.
type RPCFileUpdateChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Diff string `json:"diff"`
}

// RPCOutputDeltaNotification mirrors output text deltas for an item.
type RPCOutputDeltaNotification struct {
	Delta  string `json:"delta"`
	ItemID string `json:"itemId"`
}

// RPCMcpToolCallProgressNotification mirrors MCP tool progress text.
type RPCMcpToolCallProgressNotification struct {
	Message string `json:"message"`
	ItemID  string `json:"itemId"`
}

// RPCFileChangePatchUpdatedNotification mirrors Codex patch update
// notifications.
type RPCFileChangePatchUpdatedNotification struct {
	ItemID  string                `json:"itemId"`
	Changes []RPCFileUpdateChange `json:"changes"`
}

// RPCReasoningSummaryPartAddedNotification mirrors reasoning summary part
// notifications.
type RPCReasoningSummaryPartAddedNotification struct {
	SummaryIndex int `json:"summaryIndex"`
}

// RPCReasoningTextDeltaNotification mirrors reasoning text deltas.
type RPCReasoningTextDeltaNotification struct {
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summaryIndex"`
}

// RPCThreadTokenUsageUpdatedNotification mirrors Codex token usage updates.
type RPCThreadTokenUsageUpdatedNotification struct {
	ThreadID   string              `json:"threadId"`
	TurnID     string              `json:"turnId"`
	TokenUsage RPCThreadTokenUsage `json:"tokenUsage"`
}

// RPCThreadTokenUsage mirrors aggregate and last-turn token usage.
type RPCThreadTokenUsage struct {
	Total              RPCTokenUsage `json:"total"`
	Last               RPCTokenUsage `json:"last"`
	ModelContextWindow *int          `json:"modelContextWindow"`
}

// RPCTokenUsage mirrors token counts returned by Codex.
type RPCTokenUsage struct {
	TotalTokens           int `json:"totalTokens"`
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
}
