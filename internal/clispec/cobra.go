package clispec

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// cobraCommand renders the operation as a terminal command. Flags come from
// the parameters, the positional placeholders and required count come from the
// arguments, and the run step decodes everything into a fresh input struct
// before calling the shared work function.
func (op Operation[I, P]) cobraCommand(f *cli.Factory) *cobra.Command {
	var useBuilder strings.Builder
	useBuilder.WriteString(op.Name.CLI())
	for _, arg := range op.Args {
		useBuilder.WriteString(" ")
		useBuilder.WriteString(arg.placeholder())
	}

	cmd := &cobra.Command{
		Use:     useBuilder.String(),
		Short:   op.Short,
		Long:    op.longHelp(),
		Example: strings.Join(op.Examples, "\n"),
		Args:    cobra.ExactArgs(len(op.Args)),
	}

	applies := make([]func(in *I), 0, len(op.Params))
	for _, param := range op.Params {
		applies = append(applies, registerFlag(cmd, param))
	}

	// Prepare runs in PreRunE, which cobra invokes before RunE. The help
	// renderer wraps only RunE to silence usage, so a Prepare error here keeps
	// SilenceUsage false and cobra prints full command help, exactly as it does
	// for a missing positional argument. The prepared payload is stashed for
	// RunE, which is reached only after PreRunE returns nil; the command runs
	// once, so the closure variable is safe.
	var prepared P
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		in := op.New()
		for i, arg := range op.Args {
			arg.bind(&in, args[i])
		}
		for _, apply := range applies {
			apply(&in)
		}
		payload, err := op.Prepare(in)
		if err != nil {
			return logFail(cmd.Context(), SurfaceCLI, "invalid_input", op.Name.Canonical, err)
		}
		prepared = payload
		return nil
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		sink := NewCLISink(cmd.Context(), f.IOStreams.Out)
		return op.Run(cmd.Context(), prepared, SurfaceCLI, sink)
	}
	for _, child := range op.Children {
		if !child.surfaceSet().CLI {
			continue
		}
		cmd.AddCommand(child.cobraCommand(f))
	}
	return cmd
}

// longHelp builds the full description shown by `--help`. It starts from the
// author's Long text, or the one-line Short when Long is empty, and appends a
// documented Arguments section for every positional input so the reader sees
// what each placeholder means without leaving the help screen.
func (op Operation[I, P]) longHelp() string {
	var b strings.Builder
	switch {
	case op.Long != "":
		b.WriteString(op.Long)
	case op.Short != "":
		b.WriteString(op.Short)
	}
	if len(op.Args) > 0 {
		width := 0
		for _, arg := range op.Args {
			if len(arg.placeholder()) > width {
				width = len(arg.placeholder())
			}
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Arguments:")
		for _, arg := range op.Args {
			fmt.Fprintf(&b, "\n  %-*s  %s", width, arg.placeholder(), arg.Description)
		}
	}
	return b.String()
}

// registerFlag registers one parameter as a terminal flag and returns a
// closure that copies the parsed value into the input struct after cobra has
// parsed the command line.
func registerFlag[I Input](cmd *cobra.Command, param Param[I]) func(in *I) {
	switch param.Kind {
	case KindEnum:
		current := param.DefaultStr
		shim := &enumValue{allowed: param.Values, value: &current}
		cmd.Flags().Var(shim, param.flagName(), param.Description)
		bind := param.bindString
		return func(in *I) { bind(in, current) }
	case KindString:
		holder := new(string)
		cmd.Flags().StringVar(holder, param.flagName(), param.DefaultStr, param.Description)
		bind := param.bindString
		return func(in *I) { bind(in, *holder) }
	case KindInt:
		holder := new(int)
		cmd.Flags().IntVar(holder, param.flagName(), param.DefaultInt, param.Description)
		bind := param.bindInt
		return func(in *I) { bind(in, *holder) }
	case KindBool:
		holder := new(bool)
		cmd.Flags().BoolVar(holder, param.flagName(), param.DefaultBool, param.Description)
		bind := param.bindBool
		return func(in *I) { bind(in, *holder) }
	case KindFloat:
		holder := new(float64)
		cmd.Flags().Float64Var(holder, param.flagName(), param.DefaultFloat, param.Description)
		bind := param.bindFloat
		return func(in *I) { bind(in, *holder) }
	case KindEnumList:
		holder := new([]string)
		shim := &sliceEnumValue{allowed: param.Values, values: holder}
		cmd.Flags().Var(shim, param.flagName(), param.Description)
		bind := param.bindStrSlice
		return func(in *I) { bind(in, *holder) }
	default:
		return func(in *I) {}
	}
}

// enumValue is a pflag.Value that rejects a word outside its allowed list. It
// gives the terminal strict validation; the MCP tool decodes the same input
// leniently and falls back to the default instead.
type enumValue struct {
	allowed []string
	value   *string
}

// String returns the current value.
func (e *enumValue) String() string {
	if e.value == nil {
		return ""
	}
	return *e.value
}

// Set accepts a word in the allowed list and rejects anything else.
func (e *enumValue) Set(raw string) error {
	candidate := strings.TrimSpace(raw)
	if enumContains(e.allowed, candidate) {
		*e.value = candidate
		return nil
	}
	return fmt.Errorf("unsupported value %q (allowed: %s)", raw, strings.Join(e.allowed, ", "))
}

// Type names the flag value kind shown in help output. For an enum it lists
// the accepted words, so `--format` reads as `--format markdown|html|json|...`
// and the help screen shows the constraint inline next to the default.
func (e *enumValue) Type() string {
	if len(e.allowed) > 0 {
		return strings.Join(e.allowed, "|")
	}
	return "string"
}

// sliceEnumValue is a pflag.Value that accumulates list elements, each validated
// against an allowed set. One flag may carry a comma-separated list, and the
// flag may repeat; elements accumulate across both forms.
type sliceEnumValue struct {
	allowed []string
	values  *[]string
}

// String renders the accumulated elements as a comma-separated list.
func (v *sliceEnumValue) String() string {
	if v.values == nil {
		return ""
	}
	return strings.Join(*v.values, ",")
}

// Set splits the raw value on commas and appends each allowed element, rejecting
// the first element outside the allowed set.
func (v *sliceEnumValue) Set(raw string) error {
	for part := range strings.SplitSeq(raw, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "" {
			continue
		}
		if !enumContains(v.allowed, candidate) {
			return fmt.Errorf("unsupported value %q (allowed: %s)", candidate, strings.Join(v.allowed, ", "))
		}
		*v.values = append(*v.values, candidate)
	}
	return nil
}

// Type names the flag value kind shown in help output, listing the accepted
// words so the constraint shows inline.
func (v *sliceEnumValue) Type() string {
	if len(v.allowed) > 0 {
		return strings.Join(v.allowed, "|")
	}
	return "strings"
}
