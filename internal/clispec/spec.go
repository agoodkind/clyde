// Package clispec declares each Clyde operation once and renders it onto
// both front ends: a spf13/cobra terminal command and an mcp-go tool.
//
// One operation is one [Operation] value. The value carries the name, the
// help text, the inputs, and one work function. The work function runs the
// same way for both front ends; it never sees cobra or mcp types. The cobra
// adapter and the mcp adapter each decode their own request into the same
// typed input struct, then call that one function.
//
// The package imports goodkind.io/clyde/internal/cli for the dependency
// factory and goodkind.io/clyde/internal/conversation for the domain calls.
// It does not import internal/cli/conversation or internal/mcpserver, so the
// two surface packages import this package without an import cycle.
package clispec

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// Surface identifies which front end is invoking a work function. A work
// function reads the surface only at the points where the two front ends
// behave differently, such as writing a file versus returning text.
type Surface uint8

const (
	// SurfaceCLI marks a call that arrived through the terminal command.
	SurfaceCLI Surface = iota
	// SurfaceMCP marks a call that arrived through the MCP tool.
	SurfaceMCP
)

// SurfaceSet records which front ends an operation renders to. The six
// conversation operations set both. The hand-written operational commands
// are terminal-only and are not modeled as operations at all.
type SurfaceSet struct {
	CLI bool
	MCP bool
}

// Name carries one canonical operation identity and derives both spellings.
// The MCP tool name is always the canonical form with the clyde_ prefix. The
// terminal command name defaults to the dash-spelled canonical but an
// operation can override it with a shorter verb that reads better under a
// parent command, for example "get" under a "conversation" parent rather than
// "get-conversation" at the root.
type Name struct {
	// Canonical is snake_case without the clyde_ prefix, e.g. get_conversation.
	Canonical string
	// CLIOverride is an optional short terminal verb. Empty means derive from
	// Canonical by replacing underscores with dashes.
	CLIOverride string
}

// CLI returns the terminal command name. It uses CLIOverride when set and
// otherwise derives the name from Canonical.
func (n Name) CLI() string {
	if n.CLIOverride != "" {
		return n.CLIOverride
	}
	return strings.ReplaceAll(n.Canonical, "_", "-")
}

// MCP returns the underscore-spelled, prefixed tool name, e.g.
// clyde_get_conversation.
func (n Name) MCP() string {
	return "clyde_" + n.Canonical
}

// Group declares a terminal parent command that one or more operations attach
// under. Operations sharing the same *Group value land as subcommands of one
// rendered parent.
type Group struct {
	Use   string
	Short string
	Long  string
	// Parent is the enclosing group, or nil for a top-level group. A non-nil
	// Parent nests this group one level deeper, so a chain renders as nested
	// terminal parents, for example mitm -> trust -> install.
	Parent *Group
}

// Input is the constraint for per-operation input structs. Each input type
// implements the unexported marker method, so the generic machinery names a
// concrete constraint rather than the empty interface. This mirrors how the
// livetrack registry constrains its meta type parameter with a named marker
// interface instead of any.
type Input interface {
	isClispecInput()
}

// Operation is one declared command. I is the per-operation input struct that
// both adapters decode into. New returns a zeroed input pre-populated with
// defaults. Run is the single shared work function. Group is nil for a
// root-level operation and otherwise points at the parent the operation
// attaches under.
type Operation[I Input] struct {
	Name     Name
	Group    *Group
	Surfaces SurfaceSet
	Short    string
	Long     string
	// Examples are complete command lines shown in terminal help and appended
	// to the MCP tool description. Each entry is one runnable invocation.
	Examples []string
	Args     []Arg[I]
	Params   []Param[I]
	New      func() I
	Run      func(ctx context.Context, in I, surface Surface, sink ResultSink) error
}

// renderable is the type-erased view of an [Operation]. Concrete
// Operation[I] values satisfy it; the type parameter I stays captured inside
// the methods and never escapes as any. This mirrors how the output package's
// Payload interface and the livetrack registry erase concrete types behind a
// closed interface rather than through map[string]any.
type renderable interface {
	surfaceSet() SurfaceSet
	group() *Group
	cobraCommand(f *cli.Factory) *cobra.Command
	mcpTool() (mcp.Tool, server.ToolHandlerFunc)
}

func (op Operation[I]) surfaceSet() SurfaceSet { return op.Surfaces }
func (op Operation[I]) group() *Group          { return op.Group }

// Registry is the one ordered list of every command. It holds two entry
// kinds: full operations that render to both front ends, and pointers to
// hand-written terminal commands that carry their own dependency wiring.
type Registry struct {
	ops         []renderable
	handwritten []HandwrittenCommand
}

// HandwrittenCommand wraps an existing NewCmd-style constructor so a bespoke
// terminal command lives in the same registry as the spec operations without
// being forced through the typed input model. These commands are terminal
// only; they render no MCP tool.
type HandwrittenCommand struct {
	Build func(f *cli.Factory) *cobra.Command
}

// Register adds one operation to the registry. It is a free function rather
// than a method because Go methods cannot introduce their own type parameter.
func Register[I Input](r *Registry, op Operation[I]) {
	r.ops = append(r.ops, op)
}

// AddHandwritten adds one bespoke terminal command to the registry.
func (r *Registry) AddHandwritten(h HandwrittenCommand) {
	r.handwritten = append(r.handwritten, h)
}
