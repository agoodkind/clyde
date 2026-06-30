package output

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// newRootWithChild builds the same wiring shape as the production root: a
// persistent --output-format flag at the root and a child subcommand that
// inherits the flag without re-declaring it.
func newRootWithChild() (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "test-root"}
	PersistentFlag(root)
	child := &cobra.Command{
		Use:  "child",
		RunE: func(c *cobra.Command, args []string) error { return nil },
	}
	root.AddCommand(child)
	return root, child
}

// resolveFormat reads the inherited --output-format flag from cmd and parses
// it, mirroring how a subcommand resolves the output format.
func resolveFormat(t *testing.T, cmd *cobra.Command) (Format, error) {
	t.Helper()
	raw, err := cmd.Flags().GetString(FlagName)
	if err != nil {
		t.Fatalf("GetString(%q): %v", FlagName, err)
	}
	return ParseFormat(raw)
}

func TestOutputFormatDefaultIsText(t *testing.T) {
	root, child := newRootWithChild()
	root.SetArgs([]string{"child"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute returned err: %v", err)
	}
	format, err := resolveFormat(t, child)
	if err != nil {
		t.Fatalf("resolveFormat returned err: %v", err)
	}
	if format != FormatText {
		t.Fatalf("default Format = %q, want %q", format, FormatText)
	}
}

func TestOutputFormatJSONOnEitherSideOfSubcommand(t *testing.T) {
	cases := [][]string{
		{"--output-format", "json", "child"},
		{"child", "--output-format", "json"},
		{"--output-format=json", "child"},
		{"child", "--output-format=json"},
	}
	for _, args := range cases {
		root, child := newRootWithChild()
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("root.Execute(%v) returned err: %v", args, err)
		}
		format, err := resolveFormat(t, child)
		if err != nil {
			t.Fatalf("resolveFormat(%v) returned err: %v", args, err)
		}
		if format != FormatJSON {
			t.Fatalf("Format for %v = %q, want %q", args, format, FormatJSON)
		}
	}
}

func TestOutputFormatUnknownValueIsTypedError(t *testing.T) {
	root, child := newRootWithChild()
	root.SetArgs([]string{"--output-format", "banana", "child"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute returned err: %v", err)
	}
	_, err := resolveFormat(t, child)
	if err == nil {
		t.Fatal("resolveFormat with banana returned nil error")
	}
	var typed *UnknownFormatError
	if !errors.As(err, &typed) {
		t.Fatalf("err = %T (%v), want *UnknownFormatError", err, err)
	}
}
