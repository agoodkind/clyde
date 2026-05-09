package anthmode_test

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
)

// TestWireCaptureModeValidate covers the closed enum boundary plus the empty
// string contract so callers can rely on Validate accepting the unset value
// and rejecting any unknown text without resorting to a bare-string switch.
func TestWireCaptureModeValidate(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		input     anthmode.WireCaptureMode
		expectErr bool
	}
	cases := []testCase{
		{name: "empty accepted", input: "", expectErr: false},
		{name: "off accepted", input: anthmode.WireCaptureOff, expectErr: false},
		{name: "summary_only accepted", input: anthmode.WireCaptureSummaryOnly, expectErr: false},
		{name: "full accepted", input: anthmode.WireCaptureFull, expectErr: false},
		{name: "reasoning_only rejected", input: anthmode.WireCaptureMode("reasoning_only"), expectErr: true},
		{name: "made_up rejected", input: anthmode.WireCaptureMode("made_up"), expectErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.input.Validate()
			gotErr := err != nil
			if gotErr != testCase.expectErr {
				t.Fatalf("Validate(%q) err=%v, expectErr=%v", testCase.input, err, testCase.expectErr)
			}
			if gotErr && !strings.Contains(err.Error(), "wire_capture.mode") {
				t.Fatalf("Validate(%q) error message %q does not mention wire_capture.mode", testCase.input, err.Error())
			}
		})
	}
}

// TestWireCaptureModeResolved confirms the empty default contract maps to
// [anthmode.WireCaptureOff] and every legal value passes through unchanged.
func TestWireCaptureModeResolved(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name     string
		input    anthmode.WireCaptureMode
		expected anthmode.WireCaptureMode
	}
	cases := []testCase{
		{name: "empty resolves to off", input: "", expected: anthmode.WireCaptureOff},
		{name: "off passes through", input: anthmode.WireCaptureOff, expected: anthmode.WireCaptureOff},
		{name: "summary_only passes through", input: anthmode.WireCaptureSummaryOnly, expected: anthmode.WireCaptureSummaryOnly},
		{name: "full passes through", input: anthmode.WireCaptureFull, expected: anthmode.WireCaptureFull},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.input.Resolved(); got != testCase.expected {
				t.Fatalf("Resolved(%q) = %q, want %q", testCase.input, got, testCase.expected)
			}
		})
	}
}

// TestInboundThinkingValidate covers the closed enum boundary plus the empty
// string contract so config can rely on the validator accepting unset values
// and rejecting any unknown text.
func TestInboundThinkingValidate(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		input     anthmode.InboundThinking
		expectErr bool
	}
	cases := []testCase{
		{name: "empty accepted", input: "", expectErr: false},
		{name: "native accepted", input: anthmode.InboundThinkingNative, expectErr: false},
		{name: "drop accepted", input: anthmode.InboundThinkingDrop, expectErr: false},
		{name: "plain_text_concat accepted", input: anthmode.InboundThinkingPlainText, expectErr: false},
		{name: "passthrough accepted", input: anthmode.InboundThinkingPassthrough, expectErr: false},
		{name: "made_up rejected", input: anthmode.InboundThinking("made_up"), expectErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.input.Validate()
			gotErr := err != nil
			if gotErr != testCase.expectErr {
				t.Fatalf("Validate(%q) err=%v, expectErr=%v", testCase.input, err, testCase.expectErr)
			}
			if gotErr && !strings.Contains(err.Error(), "reasoning.inbound_thinking") {
				t.Fatalf("Validate(%q) error message %q does not mention reasoning.inbound_thinking", testCase.input, err.Error())
			}
		})
	}
}

// TestInboundThinkingResolved confirms the empty default contract maps to
// [anthmode.InboundThinkingNative] and every legal value passes through.
func TestInboundThinkingResolved(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name     string
		input    anthmode.InboundThinking
		expected anthmode.InboundThinking
	}
	cases := []testCase{
		{name: "empty resolves to native", input: "", expected: anthmode.InboundThinkingNative},
		{name: "native passes through", input: anthmode.InboundThinkingNative, expected: anthmode.InboundThinkingNative},
		{name: "drop passes through", input: anthmode.InboundThinkingDrop, expected: anthmode.InboundThinkingDrop},
		{name: "plain_text_concat passes through", input: anthmode.InboundThinkingPlainText, expected: anthmode.InboundThinkingPlainText},
		{name: "passthrough passes through", input: anthmode.InboundThinkingPassthrough, expected: anthmode.InboundThinkingPassthrough},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.input.Resolved(); got != testCase.expected {
				t.Fatalf("Resolved(%q) = %q, want %q", testCase.input, got, testCase.expected)
			}
		})
	}
}

// TestParentPackageAliasesIdentity confirms the parent anthropic package's
// type alias and constant alias surface keep referring to the same canonical
// constants in this package, so external callers using either name continue
// to compare equal.
func TestWireValuesAreStableStrings(t *testing.T) {
	t.Parallel()

	type entry struct {
		name     string
		got      string
		expected string
	}
	cases := []entry{
		{name: "WireCaptureOff", got: string(anthmode.WireCaptureOff), expected: "off"},
		{name: "WireCaptureSummaryOnly", got: string(anthmode.WireCaptureSummaryOnly), expected: "summary_only"},
		{name: "WireCaptureFull", got: string(anthmode.WireCaptureFull), expected: "full"},
		{name: "InboundThinkingNative", got: string(anthmode.InboundThinkingNative), expected: "native_thinking_block"},
		{name: "InboundThinkingDrop", got: string(anthmode.InboundThinkingDrop), expected: "drop"},
		{name: "InboundThinkingPlainText", got: string(anthmode.InboundThinkingPlainText), expected: "plain_text_concat"},
		{name: "InboundThinkingPassthrough", got: string(anthmode.InboundThinkingPassthrough), expected: "passthrough"},
	}
	for _, c := range cases {
		if c.got != c.expected {
			t.Fatalf("%s wire value drifted: got %q want %q", c.name, c.got, c.expected)
		}
	}
}
