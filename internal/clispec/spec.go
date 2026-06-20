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

type surfaceContextKey struct{}

func withSurface(ctx context.Context, surface Surface) context.Context {
	return context.WithValue(ctx, surfaceContextKey{}, surface)
}

func surfaceFromContext(ctx context.Context) Surface {
	surface, ok := ctx.Value(surfaceContextKey{}).(Surface)
	if !ok {
		return SurfaceCLI
	}
	return surface
}

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
	Use     string
	Short   string
	Long    string
	Example string
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

// Prepared is the constraint for per-operation prepared payloads, the value the
// work function receives after Prepare turns the raw input into it. Like Input
// it is a named marker rather than any, so the generic signatures name a
// concrete constraint. A payload type that only the work function reads (it
// never crosses an external boundary) is a small struct in this package; a
// payload that wraps a domain options type embeds or carries it.
type Prepared interface {
	isClispecPrepared()
}

// Operation is one declared command. I is the per-operation raw input struct
// that both adapters decode into. P is the prepared payload the work function
// receives. New returns a zeroed input pre-populated with defaults. Prepare is
// the single place that turns raw input into the prepared payload and the only
// place that may reject input: it runs before the work on both surfaces (in
// cobra PreRunE on the terminal, before the work function on MCP), so any input
// error renders full command help on the terminal and becomes tool-error text
// on MCP. Run is the shared work function; it receives only the prepared payload
// and so cannot reject input late. Group is nil for a root-level operation and
// otherwise points at the parent the operation attaches under.
type Operation[I Input, P Prepared] struct {
	Name     Name
	Group    *Group
	Surfaces SurfaceSet
	// outputKind is required when runResult or mcpTaskResult is set. Legacy
	// sink-based operations leave it as zero until they migrate.
	outputKind resultKind
	Short      string
	Long       string
	// Examples are complete command lines shown in terminal help and appended
	// to the MCP tool description. Each entry is one runnable invocation.
	Examples []string
	Args     []Arg[I]
	Params   []Param[I]
	New      func() I
	// Children are terminal subcommands mounted under this operation's terminal
	// command. They are still ordinary operations, so each child owns its input,
	// preparation, help, and work function in the registry model.
	Children []renderable
	// Prepare turns the decoded raw input into the prepared payload, returning an
	// error for any input it rejects. It must be pure: no side effects, no I/O.
	// It is the only function that sees raw input, so it is the only place input
	// can be rejected, and it always runs before the help boundary on the
	// terminal and before the work on MCP.
	Prepare func(in I) (P, error)
	// Run is the legacy sink-based work function. New result-based operations
	// leave this nil and use runResult instead.
	Run func(ctx context.Context, in P, surface Surface, sink ResultSink) error
	// runResult is the new result-based work function. When set, the central CLI
	// and MCP renderers own serialization.
	runResult func(ctx context.Context, in P) (Result, error)
	// MCPTaskSupport, when set to optional or required, marks the rendered MCP
	// tool as task-augmentable so a Tasks-capable client can run it as an MCP
	// task. It is MCP-only: the terminal command ignores it. The zero value
	// leaves the tool a plain synchronous tool.
	MCPTaskSupport mcp.TaskSupport
	// MCPTaskRun is the work function used when an MCP call is task-augmented
	// (the client supplied task params). It typically runs the operation to
	// completion so the result reaches the client through tasks/result, whereas
	// Run returns immediately. It is only consulted when MCPTaskSupport is set
	// and the request carries task params. Like Run, it receives the prepared
	// payload.
	MCPTaskRun func(ctx context.Context, in P, sink ResultSink) error
	// mcpTaskResult is the result-based task path used by migrated tools.
	mcpTaskResult func(ctx context.Context, in P) (Result, error)
}

// renderable is the type-erased view of an [Operation]. Concrete
// Operation[I, P] values satisfy it; the type parameters I and P stay captured
// inside the methods and never escape as any. This mirrors how the output
// package's Payload interface and the livetrack registry erase concrete types
// behind a closed interface rather than through map[string]any.
type renderable interface {
	surfaceSet() SurfaceSet
	group() *Group
	children() []renderable
	cobraCommand(f *cli.Factory) *cobra.Command
	mcpTool() (mcp.Tool, server.ToolHandlerFunc)
}

func (op Operation[I, P]) surfaceSet() SurfaceSet { return op.Surfaces }
func (op Operation[I, P]) group() *Group          { return op.Group }
func (op Operation[I, P]) children() []renderable { return op.Children }

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
// The type system already forces an operation to name a prepared payload P and a
// work function that takes only P, so input can be rejected only in Prepare. A
// nil Prepare would fail every invocation at the prepare step; a test over the
// operation constructors asserts none is nil, so the gap is caught in CI without
// a production panic.
func Register[I Input, P Prepared](r *Registry, op Operation[I, P]) {
	r.ops = append(r.ops, op)
}

// AddHandwritten adds one bespoke terminal command to the registry.
func (r *Registry) AddHandwritten(h HandwrittenCommand) {
	r.handwritten = append(r.handwritten, h)
}
