// Package hookspec declares Clyde hooks once, then renders them into client
// settings and runtime dispatch.
package hookspec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// HookID is the stable identifier for one Clyde-managed hook.
type HookID string

const (
	// HookIDReorientBeforeCompact captures Clyde reorient evidence before a
	// client compacts the current conversation.
	HookIDReorientBeforeCompact HookID = "reorient-before-compact"
	// HookIDReorientAfterCompact injects a previously captured reorient snapshot
	// after a client restarts from compaction.
	HookIDReorientAfterCompact HookID = "reorient-after-compact"
	// HookIDReorientStopFollowup replays a captured snapshot through a stop hook
	// follow-up message.
	HookIDReorientStopFollowup HookID = "reorient-stop-followup"
	// HookIDClaudeCodeReorientAfterCompact injects Clyde reorient evidence after
	// Claude Code compacts a session.
	HookIDClaudeCodeReorientAfterCompact HookID = "claude-code-reorient-after-compact"
)

// Client names a hook-capable client.
type Client string

const (
	// ClientAll installs every supported user-scoped hook client.
	ClientAll Client = "all"
	// ClientClaudeCode installs hooks into Claude Code user settings.
	ClientClaudeCode Client = "claude-code"
	// ClientCodex installs hooks into Codex user settings.
	ClientCodex Client = "codex"
	// ClientCursor installs hooks into Cursor user settings.
	ClientCursor Client = "cursor"
)

// SupportedClients is the stable install order for --client all.
func SupportedClients() []Client {
	return []Client{ClientClaudeCode, ClientCodex, ClientCursor}
}

// ClaudeCodeEvent names a Claude Code hook lifecycle event.
type ClaudeCodeEvent string

const (
	// ClaudeCodeEventSessionStart fires when Claude Code starts, resumes, clears,
	// or restarts after compaction.
	ClaudeCodeEventSessionStart ClaudeCodeEvent = "SessionStart"
)

// Hook event names used by supported clients.
const (
	EventPreCompact   = "PreCompact"
	EventSessionStart = string(ClaudeCodeEventSessionStart)
	EventCursorPre    = "preCompact"
	EventCursorStop   = "stop"
)

// ClaudeCodeHook declares the Claude Code settings entry for one hook.
type ClaudeCodeHook struct {
	Event          ClaudeCodeEvent
	Matcher        string
	Args           []string
	TimeoutSeconds int
	StatusMessage  string
}

// InstallSpec declares one client-specific settings entry for a hook.
type InstallSpec struct {
	Client         Client
	Event          string
	Matcher        string
	Args           []string
	TimeoutSeconds int
	StatusMessage  string
	FailClosed     bool
	LoopLimit      int
}

// RegisteredInstall binds one generic hook id to one client-specific install
// shape.
type RegisteredInstall struct {
	HookID HookID
	Spec   InstallSpec
}

// Hook declares one Clyde-managed hook.
type Hook struct {
	ID         HookID
	Client     Client
	ClaudeCode ClaudeCodeHook
	Run        Handler
}

// Handler runs one installed hook at runtime.
type Handler func(ctx context.Context, env RunEnvironment) error

// Registry stores the hook declarations that Clyde can install and run.
type Registry struct {
	hooks []Hook
}

// NewRegistry returns Clyde's built-in hooks registry.
func NewRegistry() Registry {
	registry := Registry{hooks: nil}
	if err := Register(&registry, claudeCodeReorientHook()); err != nil {
		slog.Error("hooks registry initialization failed", "err", err)
	}
	return registry
}

// Register appends one hook declaration and rejects duplicate ids.
func Register(registry *Registry, hook Hook) error {
	if registry == nil {
		return fmt.Errorf("hooks registry is nil")
	}
	if strings.TrimSpace(string(hook.ID)) == "" {
		return fmt.Errorf("hook id is required")
	}
	if _, ok := registry.Hook(hook.ID); ok {
		return fmt.Errorf("hook %q already registered", hook.ID)
	}
	registry.hooks = append(registry.hooks, hook)
	return nil
}

// Hook returns one hook declaration by id.
func (registry Registry) Hook(id HookID) (Hook, bool) {
	for _, hook := range registry.hooks {
		if hook.ID == id {
			return hook, true
		}
	}
	return Hook{
		ID:     "",
		Client: "",
		ClaudeCode: ClaudeCodeHook{
			Event:          "",
			Matcher:        "",
			Args:           nil,
			TimeoutSeconds: 0,
			StatusMessage:  "",
		},
		Run: nil,
	}, false
}

// HooksForClient returns every registered hook for one client.
func (registry Registry) HooksForClient(client Client) []Hook {
	hooks := make([]Hook, 0, len(registry.hooks))
	for _, hook := range registry.hooks {
		if hook.Client == client {
			hooks = append(hooks, hook)
		}
	}
	return hooks
}

func claudeCodeReorientHook() Hook {
	return Hook{
		ID:     HookIDClaudeCodeReorientAfterCompact,
		Client: ClientClaudeCode,
		ClaudeCode: ClaudeCodeHook{
			Event:          ClaudeCodeEventSessionStart,
			Matcher:        "compact",
			Args:           []string{"hooks", "run", string(HookIDClaudeCodeReorientAfterCompact)},
			TimeoutSeconds: 600,
			StatusMessage:  "Recovering Clyde reorient context",
		},
		Run: runClaudeCodeReorientAfterCompact,
	}
}
