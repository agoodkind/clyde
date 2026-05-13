package compact

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/contextcount"
)

type fakeContextCounter struct {
	calls          atomic.Int32
	lastTranscript contextcount.Transcript
	tokens         func(call int32, transcript contextcount.Transcript) int
	source         contextcount.CounterSource
}

func (f *fakeContextCounter) Count(
	_ context.Context,
	transcript contextcount.Transcript,
) (int, error) {
	call := f.calls.Add(1)
	f.lastTranscript = cloneContextTranscript(transcript)
	if f.tokens == nil {
		return 100, nil
	}
	return f.tokens(call, transcript), nil
}

func (f *fakeContextCounter) Source() contextcount.CounterSource {
	if f.source == "" {
		return contextcount.CounterSource("test")
	}
	return f.source
}

func TestRunPlanUsesContextCounterTranscript(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		userText("u1", "b1", "user message", t0.Add(time.Second)),
		assistantBlocks("a1", "u1", t0.Add(2*time.Second), []map[string]any{
			{"type": "thinking", "thinking": "private"},
			{"type": "text", "text": "reply"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	fake := &fakeContextCounter{
		tokens: func(call int32, transcript contextcount.Transcript) int {
			if call == 1 {
				return 1_000
			}
			if call == 2 {
				return 700
			}
			for _, message := range transcript.Messages {
				for _, block := range message.Content {
					if block.Type == "thinking" {
						t.Fatalf("thinking block survived drop transform")
					}
				}
			}
			return 200
		},
		source: contextcount.CounterSourceAPI,
	}
	res, err := RunPlan(context.Background(), PlanInput{
		Slice:          slice,
		Transcript:     contextcount.Transcript{Model: "test-model", System: nil, Tools: nil, Messages: nil},
		Strippers:      Strippers{Thinking: true, Images: true, Tools: true, Chat: true},
		Target:         500,
		StaticOverhead: 0,
		Reserved:       0,
		Counter:        fake,
		Out:            nil,
		OnIteration:    nil,
		BatchSize:      1,
		StopTimeout:    0,
		CompactRunID:   "",
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.BaselineTail != 1_000 {
		t.Errorf("BaselineTail = %d, want 1000", res.BaselineTail)
	}
	if fake.calls.Load() < 2 {
		t.Errorf("context counter calls = %d, want at least 2", fake.calls.Load())
	}
	if len(res.Iterations) == 0 {
		t.Fatalf("iterations empty")
	}
	last := res.Iterations[len(res.Iterations)-1]
	if last.CtxTotal != last.TailTokens {
		t.Fatalf("CtxTotal = %d, TailTokens = %d, want matching counter totals", last.CtxTotal, last.TailTokens)
	}
}
