package contextcount

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	generic "goodkind.io/clyde/internal/contextcount"
)

func TestParseProbeMessagesTokens(t *testing.T) {
	t.Parallel()

	output := `
╭────────────────────────────────────╮
│ Context                            │
├────────────────────┬───────────────┤
│ Messages           │ 1,234         │
│ Free space         │ 900           │
╰────────────────────┴───────────────╯
`
	tokens, err := readProbeMessagesTokens(output)
	if err != nil {
		t.Fatalf("readProbeMessagesTokens() error = %v", err)
	}
	if tokens != 1234 {
		t.Fatalf("readProbeMessagesTokens() = %d, want 1234", tokens)
	}
}

func TestProbeCounterExecError(t *testing.T) {
	t.Parallel()

	counter := NewProbeCounter(ProbeCounterOptions{
		Binary:  "fake-claude",
		WorkDir: "",
		Timeout: time.Second,
		Exec: func(context.Context, probeCommand) (probeOutput, error) {
			return probeOutput{Stdout: "", Stderr: "failed"}, errors.New("spawn failed")
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	_, err := counter.Count(context.Background(), generic.Transcript{
		Model:    "claude-haiku-4-5",
		System:   nil,
		Tools:    nil,
		Messages: nil,
	})
	if err == nil {
		t.Fatalf("Count() error = nil, want exec error")
	}
}

func TestProbeCounterRegexNoMatch(t *testing.T) {
	t.Parallel()

	counter := NewProbeCounter(ProbeCounterOptions{
		Binary:  "fake-claude",
		WorkDir: "",
		Timeout: time.Second,
		Exec: func(context.Context, probeCommand) (probeOutput, error) {
			return probeOutput{Stdout: "no context table here", Stderr: ""}, nil
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	_, err := counter.Count(context.Background(), generic.Transcript{
		Model:    "claude-haiku-4-5",
		System:   nil,
		Tools:    nil,
		Messages: nil,
	})
	if err == nil {
		t.Fatalf("Count() error = nil, want parse error")
	}
}

func TestProbeCounterContractRoundTrip(t *testing.T) {
	t.Parallel()

	var recorded probeCommand
	var counter generic.Counter = NewProbeCounter(ProbeCounterOptions{
		Binary:  "fake-claude",
		WorkDir: "/tmp/work",
		Timeout: time.Second,
		Exec: func(_ context.Context, command probeCommand) (probeOutput, error) {
			recorded = command
			return probeOutput{
				Stdout: "| Messages | 987 |\n",
				Stderr: "",
			}, nil
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	tokens, err := counter.Count(context.Background(), generic.Transcript{
		Model:    "claude-haiku-4-5",
		System:   nil,
		Tools:    nil,
		Messages: nil,
	})
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 987 {
		t.Fatalf("Count() = %d, want 987", tokens)
	}
	if counter.Source() != generic.CounterSourceProbe {
		t.Fatalf("Source() = %q, want %q", counter.Source(), generic.CounterSourceProbe)
	}
	wantArgs := []string{"-p", "/context", "--no-session-persistence"}
	if !reflect.DeepEqual(recorded.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", recorded.Args, wantArgs)
	}
	if recorded.Binary != "fake-claude" {
		t.Fatalf("binary = %q, want fake-claude", recorded.Binary)
	}
	if recorded.WorkDir != "/tmp/work" {
		t.Fatalf("work dir = %q, want /tmp/work", recorded.WorkDir)
	}
}
