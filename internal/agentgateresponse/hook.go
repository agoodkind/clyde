// Package agentgateresponse adds deterministic agent-gate diagnostics to
// intercepted Anthropic responses.
package agentgateresponse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/mitm"
)

const (
	claudeProviderName = "claude"
	messagesPath       = "/v1/messages"
	responseTimeout    = 5 * time.Second
)

// Hook preserves the first matching MITM hook and checks the resulting
// Anthropic response text with agent-gate.
type Hook struct {
	command string
	nested  []mitm.RequestResponseHook
}

// New creates a response check around the existing MITM hooks.
func New(command string, nested []mitm.RequestResponseHook) *Hook {
	return &Hook{
		command: strings.TrimSpace(command),
		nested:  slices.Clone(nested),
	}
}

// MatchRequestResponse selects the existing hook first. Anthropic message
// responses then receive the agent-gate check after that hook finishes.
func (h *Hook) MatchRequestResponse(
	request mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	match, err := h.matchNested(request)
	if err != nil {
		return mitm.RequestResponseHookMatch{}, err
	}
	if !h.matchesAnthropicMessages(request) {
		return match, nil
	}
	checker := responseTransformer{
		command:   h.command,
		sessionID: strings.TrimSpace(request.Header.Get("X-Claude-Code-Session-Id")),
	}
	match.Matched = true
	match.Transformer = responseTransformerChain{
		first:  match.Transformer,
		second: checker,
	}
	return match, nil
}

func (h *Hook) matchNested(
	request mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	for _, nested := range h.nested {
		if nested == nil {
			continue
		}
		match, err := nested.MatchRequestResponse(request)
		if err != nil {
			slog.Warn(
				"mitm.agent_gate_response.nested_match_failed",
				"concern", "providers.mitm.wire",
				"err", err,
			)
			return mitm.RequestResponseHookMatch{}, fmt.Errorf("match nested response hook: %w", err)
		}
		if match.Matched {
			return match, nil
		}
	}
	return mitm.RequestResponseHookMatch{}, nil
}

func (h *Hook) matchesAnthropicMessages(request mitm.RequestResponseHookRequest) bool {
	if h == nil || h.command == "" {
		return false
	}
	return request.Provider == claudeProviderName &&
		request.Method == http.MethodPost &&
		request.Path == messagesPath
}

type responseTransformerChain struct {
	first  mitm.ResponseTransformer
	second mitm.ResponseTransformer
}

func (c responseTransformerChain) TransformResponse(
	ctx context.Context,
	response mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	var err error
	if c.first != nil {
		response, err = c.first.TransformResponse(ctx, response)
		if err != nil {
			slog.WarnContext(
				ctx,
				"mitm.agent_gate_response.nested_transform_failed",
				"concern", "providers.mitm.wire",
				"err", err,
			)
			return response, fmt.Errorf("transform nested response: %w", err)
		}
	}
	return c.second.TransformResponse(ctx, response)
}

type responseTransformer struct {
	command   string
	sessionID string
}

func (t responseTransformer) TransformResponse(
	ctx context.Context,
	response mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	if !isAnthropicEventStream(response) {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return responseWithBody(response, body), fmt.Errorf("read Anthropic response: %w", err)
	}
	if !bytes.Contains(body, []byte("event: message_stop")) {
		return responseWithBody(response, body), nil
	}
	text := anthropic.ResponseText(body)
	if strings.TrimSpace(text) == "" {
		return responseWithBody(response, body), nil
	}
	diagnostic, err := t.check(ctx, text)
	if err != nil {
		slog.WarnContext(
			ctx,
			"mitm.agent_gate_response.check_failed",
			"concern", "providers.mitm.wire",
			"err", err,
		)
		return responseWithBody(response, body), nil
	}
	if diagnostic == "" {
		return responseWithBody(response, body), nil
	}
	message := "Agent-gate detected a rule violation in the preceding response. " +
		"Rewrite the response before continuing.\n\n" + diagnostic
	augmented, err := anthropic.AppendTextBlock(body, message)
	if err != nil {
		return responseWithBody(response, body), fmt.Errorf("append agent-gate diagnostic: %w", err)
	}
	return responseWithBody(response, augmented), nil
}

func isAnthropicEventStream(response mitm.ResponseHookResponse) bool {
	if response.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

type claudeHookPayload struct {
	HookEventName       string          `json:"hook_event_name"`
	SessionID           string          `json:"session_id"`
	TranscriptPath      string          `json:"transcript_path"`
	CWD                 string          `json:"cwd"`
	PermissionMode      string          `json:"permission_mode"`
	ToolName            string          `json:"tool_name"`
	ToolUseID           string          `json:"tool_use_id"`
	ToolInput           claudeToolInput `json:"tool_input"`
}

type claudeToolInput struct {
	Content string `json:"content"`
}

func (t responseTransformer) check(ctx context.Context, text string) (string, error) {
	payload := claudeHookPayload{
		HookEventName:  "PreToolUse",
		SessionID:      t.sessionID,
		TranscriptPath: "",
		CWD:            "",
		PermissionMode: "default",
		ToolName:       "AgentGateResponse",
		ToolUseID:      "clyde-response",
		ToolInput:      claudeToolInput{Content: text},
	}
	input, err := json.Marshal(payload)
	if err != nil {
		slog.WarnContext(
			ctx,
			"mitm.agent_gate_response.request_encode_failed",
			"concern", "providers.mitm.wire",
			"err", err,
		)
		return "", fmt.Errorf("encode agent-gate request: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, responseTimeout)
	defer cancel()
	command := exec.CommandContext(checkCtx, t.command, "managed-hook", "claude")
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	slog.DebugContext(
		ctx,
		"mitm.agent_gate_response.check_started",
		"concern", "providers.mitm.wire",
		"response_bytes", len(text),
	)
	err = command.Run()
	if err == nil {
		return "", nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 2 {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			return "", errors.New("agent-gate returned an empty violation diagnostic")
		}
		return diagnostic, nil
	}
	slog.WarnContext(
		ctx,
		"mitm.agent_gate_response.command_failed",
		"concern", "providers.mitm.wire",
		"err", err,
	)
	return "", fmt.Errorf("run agent-gate response check: %w", err)
}

func responseWithBody(
	response mitm.ResponseHookResponse,
	body []byte,
) mitm.ResponseHookResponse {
	response.Header = response.Header.Clone()
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.Body = bytes.NewReader(body)
	return response
}
