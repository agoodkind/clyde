// Package hookspec declares Clyde hooks once, then renders them into client
// settings and runtime dispatch.
package hookspec

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	// ClaudeCodeEventPreCompact fires before Claude Code compacts a session.
	ClaudeCodeEventPreCompact ClaudeCodeEvent = "PreCompact"
	// ClaudeCodeEventSessionStart fires when Claude Code starts, resumes, clears,
	// or restarts after compaction.
	ClaudeCodeEventSessionStart ClaudeCodeEvent = "SessionStart"
)

// Hook event names used by supported clients.
const (
	EventPreCompact   = string(ClaudeCodeEventPreCompact)
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
	Aliases    []HookID
	Installs   []InstallSpec
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
	for _, hook := range []Hook{
		reorientBeforeCompactHook(),
		reorientAfterCompactHook(),
		reorientStopFollowupHook(),
	} {
		if err := Register(&registry, hook); err != nil {
			slog.Error("hooks registry initialization failed", "err", err)
		}
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
	seenAliases := map[HookID]bool{}
	for _, alias := range hook.Aliases {
		if strings.TrimSpace(string(alias)) == "" {
			return fmt.Errorf("hook %q has an empty alias", hook.ID)
		}
		if alias == hook.ID {
			return fmt.Errorf("hook %q has alias equal to hook id", hook.ID)
		}
		if seenAliases[alias] {
			return fmt.Errorf("hook %q has duplicate alias %q", hook.ID, alias)
		}
		seenAliases[alias] = true
		if _, ok := registry.Hook(alias); ok {
			return fmt.Errorf("hook alias %q already registered", alias)
		}
	}
	registry.hooks = append(registry.hooks, hook)
	return nil
}

// Hook returns one hook declaration by id or runtime alias.
func (registry Registry) Hook(id HookID) (Hook, bool) {
	for _, hook := range registry.hooks {
		if hook.ID == id || slices.Contains(hook.Aliases, id) {
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
		Aliases:  nil,
		Installs: nil,
		Run:      nil,
	}, false
}

// InstallsForClient returns every install spec for one client.
func (registry Registry) InstallsForClient(client Client) []RegisteredInstall {
	installs := make([]RegisteredInstall, 0, len(registry.hooks))
	for _, hook := range registry.hooks {
		for _, install := range hook.Installs {
			if install.Client != client {
				continue
			}
			installs = append(installs, RegisteredInstall{
				HookID: hook.ID,
				Spec:   install.clone(),
			})
		}
	}
	return installs
}

// ClydeCommandSignatures returns every current and legacy runtime args shape
// that marks a hook handler as Clyde-owned.
func (registry Registry) ClydeCommandSignatures() [][]string {
	var signatures [][]string
	for _, hook := range registry.hooks {
		for _, install := range hook.Installs {
			signatures = append(signatures, slices.Clone(install.Args))
		}
		for _, alias := range hook.Aliases {
			signatures = append(signatures, []string{"hooks", "run", string(alias)})
		}
	}
	return signatures
}

func (install InstallSpec) clone() InstallSpec {
	install.Args = slices.Clone(install.Args)
	return install
}

func reorientBeforeCompactHook() Hook {
	return Hook{
		ID:     HookIDReorientBeforeCompact,
		Client: ClientClaudeCode,
		ClaudeCode: ClaudeCodeHook{
			Event:          ClaudeCodeEventPreCompact,
			Matcher:        "",
			Args:           []string{"hooks", "run", string(HookIDReorientBeforeCompact)},
			TimeoutSeconds: 600,
			StatusMessage:  "Capturing Clyde reorient context",
		},
		Aliases: nil,
		Installs: []InstallSpec{
			{
				Client:         ClientClaudeCode,
				Event:          EventPreCompact,
				Matcher:        "",
				Args:           []string{"hooks", "run", string(HookIDReorientBeforeCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "Capturing Clyde reorient context",
				FailClosed:     false,
				LoopLimit:      0,
			},
			{
				Client:         ClientCodex,
				Event:          EventPreCompact,
				Matcher:        "",
				Args:           []string{"hooks", "run", string(HookIDReorientBeforeCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "Capturing Clyde reorient context",
				FailClosed:     false,
				LoopLimit:      0,
			},
			{
				Client:         ClientCursor,
				Event:          EventCursorPre,
				Matcher:        "",
				Args:           []string{"hooks", "run", string(HookIDReorientBeforeCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "",
				FailClosed:     false,
				LoopLimit:      0,
			},
		},
		Run: runReorientBeforeCompact,
	}
}

func reorientAfterCompactHook() Hook {
	return Hook{
		ID:     HookIDReorientAfterCompact,
		Client: ClientClaudeCode,
		ClaudeCode: ClaudeCodeHook{
			Event:          ClaudeCodeEventSessionStart,
			Matcher:        "compact",
			Args:           []string{"hooks", "run", string(HookIDClaudeCodeReorientAfterCompact)},
			TimeoutSeconds: 600,
			StatusMessage:  "Recovering Clyde reorient context",
		},
		Aliases: []HookID{HookIDClaudeCodeReorientAfterCompact},
		Installs: []InstallSpec{
			{
				Client:         ClientClaudeCode,
				Event:          EventSessionStart,
				Matcher:        "compact",
				Args:           []string{"hooks", "run", string(HookIDReorientAfterCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "Recovering Clyde reorient context",
				FailClosed:     false,
				LoopLimit:      0,
			},
			{
				Client:         ClientCodex,
				Event:          EventSessionStart,
				Matcher:        "compact",
				Args:           []string{"hooks", "run", string(HookIDReorientAfterCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "Recovering Clyde reorient context",
				FailClosed:     false,
				LoopLimit:      0,
			},
		},
		Run: runReorientAfterCompact,
	}
}

func reorientStopFollowupHook() Hook {
	return Hook{
		ID:     HookIDReorientStopFollowup,
		Client: ClientCursor,
		ClaudeCode: ClaudeCodeHook{
			Event:          "",
			Matcher:        "",
			Args:           nil,
			TimeoutSeconds: 0,
			StatusMessage:  "",
		},
		Aliases: nil,
		Installs: []InstallSpec{{
			Client:         ClientCursor,
			Event:          EventCursorStop,
			Matcher:        "",
			Args:           []string{"hooks", "run", string(HookIDReorientStopFollowup)},
			TimeoutSeconds: 600,
			StatusMessage:  "",
			FailClosed:     false,
			LoopLimit:      1,
		}},
		Run: runReorientStopFollowup,
	}
}
