package output

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// newRootWithChild builds the same wiring shape as the production
// root: a persistent --output-format flag at the root and a child
// subcommand that inherits the flag without re-declaring it.
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

func TestFrom_DefaultIsText(t *testing.T) {
	root, child := newRootWithChild()
	root.SetArgs([]string{"child"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute returned err: %v", err)
	}
	enc, err := From(child, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("From returned err: %v", err)
	}
	if enc.Format != FormatText {
		t.Fatalf("default Format = %q, want %q", enc.Format, FormatText)
	}
}

func TestFrom_JSONOnEitherSideOfSubcommand(t *testing.T) {
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
		enc, err := From(child, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("From(%v) returned err: %v", args, err)
		}
		if enc.Format != FormatJSON {
			t.Fatalf("Format for %v = %q, want %q", args, enc.Format, FormatJSON)
		}
	}
}

func TestFrom_UnknownValueIsTypedError(t *testing.T) {
	root, child := newRootWithChild()
	root.SetArgs([]string{"--output-format", "banana", "child"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute returned err: %v", err)
	}
	_, err := From(child, &bytes.Buffer{})
	if err == nil {
		t.Fatal("From with banana returned nil error")
	}
	var typed *UnknownFormatError
	if !errors.As(err, &typed) {
		t.Fatalf("err = %T (%v), want *UnknownFormatError", err, err)
	}
}
