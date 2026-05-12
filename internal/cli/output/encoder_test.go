package output

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// testPayload is a local Payload variant used only by tests in this
// package so the encoder logic can be exercised without depending on
// real CLI variants from other packages.
type testPayload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (testPayload) IsOutputPayload() {}

func TestParseFormat_KnownValues(t *testing.T) {
	cases := []struct {
		input string
		want  Format
	}{
		{"", FormatText},
		{"text", FormatText},
		{"json", FormatJSON},
	}
	for _, tc := range cases {
		got, err := ParseFormat(tc.input)
		if err != nil {
			t.Fatalf("ParseFormat(%q) returned err: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseFormat_UnknownValueTypedError(t *testing.T) {
	_, err := ParseFormat("banana")
	if err == nil {
		t.Fatal("ParseFormat(banana) returned nil error")
	}
	var typed *UnknownFormatError
	if !errors.As(err, &typed) {
		t.Fatalf("ParseFormat(banana) err = %T (%v), want *UnknownFormatError", err, err)
	}
	if typed.Input != "banana" {
		t.Fatalf("typed.Input = %q, want banana", typed.Input)
	}
	if !strings.Contains(err.Error(), "text") || !strings.Contains(err.Error(), "json") {
		t.Fatalf("err.Error() %q should list supported formats", err.Error())
	}
}

func TestEncoder_EmitJSON_WritesOneLine(t *testing.T) {
	var buf bytes.Buffer
	enc := &Encoder{Format: FormatJSON, W: &buf}
	if err := enc.Emit(testPayload{Name: "alpha", Count: 3}, nil); err != nil {
		t.Fatalf("Emit returned err: %v", err)
	}
	got := buf.String()
	want := "{\"name\":\"alpha\",\"count\":3}\n"
	if got != want {
		t.Fatalf("Emit JSON output = %q, want %q", got, want)
	}
}

func TestEncoder_EmitText_InvokesRenderer(t *testing.T) {
	var buf bytes.Buffer
	enc := &Encoder{Format: FormatText, W: &buf}
	called := false
	textFn := func(w io.Writer) error {
		called = true
		_, err := io.WriteString(w, "hello\n")
		return err
	}
	if err := enc.Emit(testPayload{Name: "alpha", Count: 3}, textFn); err != nil {
		t.Fatalf("Emit returned err: %v", err)
	}
	if !called {
		t.Fatal("text renderer not invoked in FormatText mode")
	}
	if buf.String() != "hello\n" {
		t.Fatalf("text output = %q, want %q", buf.String(), "hello\n")
	}
}

func TestEncoder_EmitText_NilRendererIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	enc := &Encoder{Format: FormatText, W: &buf}
	if err := enc.Emit(testPayload{Name: "alpha"}, nil); err != nil {
		t.Fatalf("Emit returned err: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("nil textFn in text mode wrote %q, want empty", buf.String())
	}
}

func TestEncoder_EmitJSON_SkipsTextRenderer(t *testing.T) {
	var buf bytes.Buffer
	enc := &Encoder{Format: FormatJSON, W: &buf}
	called := false
	textFn := func(w io.Writer) error {
		called = true
		return nil
	}
	if err := enc.Emit(testPayload{Name: "beta", Count: 7}, textFn); err != nil {
		t.Fatalf("Emit returned err: %v", err)
	}
	if called {
		t.Fatal("text renderer should not run in FormatJSON mode")
	}
}

func TestEncoder_Emit_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	enc := &Encoder{Format: Format("yaml"), W: &buf}
	err := enc.Emit(testPayload{}, nil)
	if err == nil {
		t.Fatal("Emit with unknown format returned nil error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Fatalf("err.Error() = %q, want it to name the unknown format", err.Error())
	}
}
