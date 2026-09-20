// Package agentgateaction evaluates assistant text with Agent Gate.
package agentgateaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"goodkind.io/clyde/internal/mitm"
)

const responseTimeout = 5 * time.Second

// Action evaluates assistant text through the Agent Gate response hook.
type Action struct {
	command string
}

// New returns an action that invokes command.
func New(command string) *Action {
	return &Action{command: strings.TrimSpace(command)}
}

type responsePayload struct {
	HookEventName    string `json:"hook_event_name"`
	SessionID        string `json:"session_id"`
	AssistantMessage string `json:"assistant_message"`
}

// EvaluateResponse returns the Agent Gate diagnostic for a violation.
func (a *Action) EvaluateResponse(
	ctx context.Context,
	input mitm.ResponseActionInput,
) (mitm.ResponseActionResult, error) {
	if a == nil || a.command == "" {
		return mitm.ResponseActionResult{}, nil
	}
	payload := responsePayload{
		HookEventName:    "Response",
		SessionID:        input.SessionID,
		AssistantMessage: input.Text,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return mitm.ResponseActionResult{}, fmt.Errorf("encode Agent Gate response event: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, responseTimeout)
	defer cancel()
	command := exec.CommandContext(checkCtx, a.command, "response-hook")
	command.Stdin = bytes.NewReader(encoded)
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	err = command.Run()
	if err == nil {
		return mitm.ResponseActionResult{}, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 2 {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			return mitm.ResponseActionResult{}, errors.New("Agent Gate returned an empty violation diagnostic")
		}
		return mitm.ResponseActionResult{Feedback: diagnostic}, nil
	}
	slog.WarnContext(
		ctx,
		"mitm.agent_gate_response.command_failed",
		"concern", "providers.mitm.wire",
		"err", err,
	)
	return mitm.ResponseActionResult{}, fmt.Errorf("run Agent Gate response hook: %w", err)
}
