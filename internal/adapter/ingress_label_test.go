package adapter

import (
	"context"
	"testing"
)

// TestIngressLabelForPort confirms the accounting split: the configured Cursor
// ingress port is tagged "cursor", every other port "openai", and an unset
// split (cursorPort 0) always reports "openai".
func TestIngressLabelForPort(t *testing.T) {
	cases := []struct {
		name       string
		cursorPort int
		localPort  int
		want       string
	}{
		{name: "cursor port matches", cursorPort: 11435, localPort: 11435, want: "cursor"},
		{name: "openai primary port", cursorPort: 11435, localPort: 11434, want: "openai"},
		{name: "split disabled", cursorPort: 0, localPort: 11435, want: "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingressLabelForPort(tc.cursorPort, tc.localPort); got != tc.want {
				t.Fatalf("ingressLabelForPort(%d, %d) = %q, want %q", tc.cursorPort, tc.localPort, got, tc.want)
			}
		})
	}
}

// TestIngressLabelFromContext round-trips the stamped label and reports empty
// when no label is present.
func TestIngressLabelFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), ingressLabelKey{}, "cursor")
	if got := ingressLabelFromContext(ctx); got != "cursor" {
		t.Fatalf("ingressLabelFromContext = %q, want cursor", got)
	}
	if got := ingressLabelFromContext(context.Background()); got != "" {
		t.Fatalf("ingressLabelFromContext (absent) = %q, want empty", got)
	}
}
