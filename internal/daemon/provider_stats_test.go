package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog/correlation"
)

func TestProviderStatsGenerationStartStaysFixedAcrossSnapshots(t *testing.T) {
	t.Parallel()
	generationStart := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	currentTime := generationStart
	recorder := newProviderStatsRecorder(adapterruntime.NewPricingTable(nil))
	recorder.generationStartedAt = generationStart
	recorder.now = func() time.Time { return currentTime }

	_, firstLoadedAt := recorder.snapshot()
	currentTime = generationStart.Add(time.Hour)
	_, secondLoadedAt := recorder.snapshot()
	currentTime = generationStart.Add(2 * time.Hour)
	recorder.record(context.Background(), completedEvent("codex", "unpriced", 0, 0, 0, 0))
	stats, thirdLoadedAt := recorder.snapshot()

	if !firstLoadedAt.Equal(generationStart) || !secondLoadedAt.Equal(generationStart) || !thirdLoadedAt.Equal(generationStart) {
		t.Fatalf("loaded times = %s, %s, %s; want fixed %s", firstLoadedAt, secondLoadedAt, thirdLoadedAt, generationStart)
	}
	if len(stats) != 1 || stats[0].LastSeenUnix != currentTime.Unix() {
		t.Fatalf("stats = %+v, want LastSeenUnix %d", stats, currentTime.Unix())
	}
}

// completedEvent builds a minimal terminal RequestEvent for the
// read-time aggregation tests. The aggregator prices ModelID plus the
// token/cache counts; the rest is zero-valued to satisfy exhaustruct.
func completedEvent(provider, modelID string, tokensIn, tokensOut, cacheRead, cacheCreate int) adapterruntime.RequestEvent {
	return adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            provider,
		ModelID:             modelID,
		TokensIn:            tokensIn,
		TokensOut:           tokensOut,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreate,
		Backend:             "", RequestID: "", Alias: "", Stream: false, FinishReason: "",
		DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false,
		DurationMs: 0, Err: "", Correlation: correlation.Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	}
}

// TestRecordComputesDollarCostFromConfigTable verifies the read-time
// dollar calc: the aggregator prices the recorded token counts of an
// anthropic completion event against the config pricing table rather
// than reading any precomputed cost off the event.
func TestRecordComputesDollarCostFromConfigTable(t *testing.T) {
	table := adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"claude-opus-4-8": {
			InputPerMTok:      5,
			OutputPerMTok:     25,
			CacheWritePerMTok: 0,
			CacheReadPerMTok:  0,
		},
	})
	recorder := newProviderStatsRecorder(table)
	ctx := context.Background()
	recorder.record(ctx, completedEvent("anthropic-oauth", "claude-opus-4-8", 1_000, 1_000, 0, 0))

	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected one provider stats entry, got %d", len(snapshot))
	}
	// 1000 * 500 input + 1000 * 2500 output = 3_000_000 microcents.
	if snapshot[0].EstimatedCostMicrocents != 3_000_000 {
		t.Fatalf("read-time cost=%d want 3_000_000", snapshot[0].EstimatedCostMicrocents)
	}
	if snapshot[0].InputTokens != 1_000 || snapshot[0].OutputTokens != 1_000 {
		t.Fatalf("token aggregation mismatch: in=%d out=%d", snapshot[0].InputTokens, snapshot[0].OutputTokens)
	}
}

// TestRecordPricesCodexFromTokens is the codex cost=0 bug-fix
// regression at the aggregation layer. The codex dispatch path no longer
// carries a precomputed cost, so the only way codex spend gets counted
// is by pricing its recorded tokens here. A configured codex model must
// therefore produce a nonzero EstimatedCostMicrocents.
func TestRecordPricesCodexFromTokens(t *testing.T) {
	table := adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"gpt-5": {
			InputPerMTok:      1.25,
			OutputPerMTok:     10,
			CacheWritePerMTok: 0,
			CacheReadPerMTok:  0.125,
		},
	})
	recorder := newProviderStatsRecorder(table)
	ctx := context.Background()
	recorder.record(ctx, completedEvent("codex", "gpt-5", 10_000, 2_000, 5_000, 0))

	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected one provider stats entry, got %d", len(snapshot))
	}
	// Input 10_000*125 + output 2_000*1000 + cache read 5_000*13 = 3_315_000.
	if snapshot[0].EstimatedCostMicrocents != 3_315_000 {
		t.Fatalf("codex read-time cost=%d want 3_315_000 (must be nonzero)", snapshot[0].EstimatedCostMicrocents)
	}
}

// TestRecordUnknownModelYieldsZeroCost confirms an event whose model id
// is not in the pricing table aggregates tokens but adds no dollar cost.
func TestRecordUnknownModelYieldsZeroCost(t *testing.T) {
	recorder := newProviderStatsRecorder(adapterruntime.NewPricingTable(nil))
	ctx := context.Background()
	recorder.record(ctx, completedEvent("anthropic-oauth", "claude-opus-4-8", 1_000, 1_000, 0, 0))

	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected one provider stats entry, got %d", len(snapshot))
	}
	if snapshot[0].EstimatedCostMicrocents != 0 {
		t.Fatalf("unknown model must add zero cost, got %d", snapshot[0].EstimatedCostMicrocents)
	}
	if snapshot[0].InputTokens != 1_000 {
		t.Fatalf("tokens should still aggregate, got %d", snapshot[0].InputTokens)
	}
}

func TestReplacePricingIsConcurrentWithTerminalCostReads(t *testing.T) {
	low := adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"gpt-hot": {InputPerMTok: 1},
	})
	high := adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"gpt-hot": {InputPerMTok: 4},
	})
	recorder := newProviderStatsRecorder(low)
	const eventCount = 1000
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		for i := range eventCount {
			if i%2 == 0 {
				recorder.replacePricing(high)
				continue
			}
			recorder.replacePricing(low)
		}
	}()
	go func() {
		defer waitGroup.Done()
		for range eventCount {
			recorder.record(context.Background(), completedEvent("codex", "gpt-hot", 1, 0, 0, 0))
		}
	}()
	waitGroup.Wait()

	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 || snapshot[0].InputTokens != eventCount {
		t.Fatalf("concurrent stats = %+v", snapshot)
	}
	minimumCost := int64(eventCount * 100)
	maximumCost := int64(eventCount * 400)
	if snapshot[0].EstimatedCostMicrocents < minimumCost || snapshot[0].EstimatedCostMicrocents > maximumCost {
		t.Fatalf("concurrent cost = %d, want [%d, %d]", snapshot[0].EstimatedCostMicrocents, minimumCost, maximumCost)
	}
}
