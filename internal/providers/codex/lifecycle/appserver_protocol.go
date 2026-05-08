package codex

import (
	"encoding/json"
	"fmt"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

type codexOutboundMessage interface {
	codexOutboundMessage()
}

type codexAppServerParams interface {
	codexAppServerParams()
}

type codexAppServerResponse interface {
	codexAppServerResponse()
}

type codexRequestEnvelope struct {
	ID     string               `json:"id"`
	Method string               `json:"method"`
	Params codexAppServerParams `json:"params"`
}

func (codexRequestEnvelope) codexOutboundMessage() {}

type codexNotificationEnvelope struct {
	Method string `json:"method"`
}

func (codexNotificationEnvelope) codexOutboundMessage() {}

type codexServerRequestResponseEnvelope struct {
	ID     string                         `json:"id"`
	Result codexServerRequestResultObject `json:"result"`
}

func (codexServerRequestResponseEnvelope) codexOutboundMessage() {}

type codexServerRequestResultObject interface {
	codexServerRequestResultObject()
}

type codexInboundMessage struct {
	ID     *string         `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e codexRPCError) asError(method string) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("codex app-server %s failed: code=%d message=%s", method, e.Code, e.Message)
	}
	return fmt.Errorf("codex app-server %s failed: code=%d message=%s data=%s", method, e.Code, e.Message, string(e.Data))
}

type codexInitializeParams struct {
	ClientInfo   codexClientInfo             `json:"clientInfo"`
	Capabilities codexInitializeCapabilities `json:"capabilities"`
}

func (codexInitializeParams) codexAppServerParams() {}

type codexClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type codexInitializeCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type codexThreadStartSource string

type codexThreadStartParams struct {
	ApprovalPolicy        string                 `json:"approvalPolicy,omitempty"`
	BaseInstructions      string                 `json:"baseInstructions,omitempty"`
	CWD                   string                 `json:"cwd,omitempty"`
	DeveloperInstructions string                 `json:"developerInstructions,omitempty"`
	Ephemeral             bool                   `json:"ephemeral,omitempty"`
	Model                 string                 `json:"model,omitempty"`
	ModelProvider         string                 `json:"modelProvider,omitempty"`
	SessionStartSource    codexThreadStartSource `json:"sessionStartSource,omitempty"`
}

func (codexThreadStartParams) codexAppServerParams() {}

type codexThreadResumeParams struct {
	ThreadID string `json:"threadId"`
}

func (codexThreadResumeParams) codexAppServerParams() {}

type codexThreadSetNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

func (codexThreadSetNameParams) codexAppServerParams() {}

type codexTurnStartParams adaptercodex.RPCTurnStartParams

func (codexTurnStartParams) codexAppServerParams() {}

type codexTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

func (codexTurnInterruptParams) codexAppServerParams() {}

type codexInitializeResponse struct {
	UserAgent      string           `json:"userAgent,omitempty"`
	ServerInfo     *codexServerInfo `json:"serverInfo,omitempty"`
	PlatformFamily string           `json:"platformFamily,omitempty"`
	PlatformOS     string           `json:"platformOs,omitempty"`
}

func (codexInitializeResponse) codexAppServerResponse() {}

type codexServerInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type codexThreadStartResponse struct {
	Thread codexThread `json:"thread"`
	CWD    string      `json:"cwd,omitempty"`
	Model  string      `json:"model,omitempty"`
}

func (codexThreadStartResponse) codexAppServerResponse() {}

type codexThreadResumeResponse struct {
	Thread codexThread `json:"thread"`
	CWD    string      `json:"cwd,omitempty"`
	Model  string      `json:"model,omitempty"`
}

func (codexThreadResumeResponse) codexAppServerResponse() {}

type codexTurnStartResponse struct {
	Turn codexTurn `json:"turn"`
}

func (codexTurnStartResponse) codexAppServerResponse() {}

type codexNoResult struct {
	Status string `json:"status,omitempty"`
}

func (codexNoResult) codexAppServerResponse() {}

type codexThread struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type codexTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type codexAgentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type codexTurnCompletedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     codexTurn `json:"turn"`
}

type codexApprovalResult struct {
	Decision string `json:"decision"`
}

func (codexApprovalResult) codexServerRequestResultObject() {}

type codexUnsupportedServerRequestResult struct {
	Ignored bool `json:"ignored"`
}

func (codexUnsupportedServerRequestResult) codexServerRequestResultObject() {}
