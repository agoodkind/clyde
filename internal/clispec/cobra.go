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
func (op Operation[I]) cobraCommand(f *cli.Factory) *cobra.Command {
	var useBuilder strings.Builder
	useBuilder.WriteString(op.Name.CLI())
	for _, arg := range op.Args {
		useBuilder.WriteString(" ")
		useBuilder.WriteString(arg.placeholder())
	}

	cmd := &cobra.Command{
		Use:   useBuilder.String(),
		Short: op.Short,
		Long:  op.Long,
		Args:  cobra.ExactArgs(len(op.Args)),
	}

	applies := make([]func(in *I), 0, len(op.Params))
	for _, param := range op.Params {
		applies = append(applies, registerFlag(cmd, param))
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in := op.New()
		for i, arg := range op.Args {
			arg.bind(&in, args[i])
		}
		for _, apply := range applies {
			apply(&in)
		}
		sink := NewCLISink(f.IOStreams.Out)
		return op.Run(cmd.Context(), in, SurfaceCLI, sink)
	}
	return cmd
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

// Type names the flag value kind shown in help output.
func (e *enumValue) Type() string {
	return "string"
}
