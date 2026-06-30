package codex

import (
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestCapabilityReportForModelUsesObservedHTTPContextForCodexResponses(t *testing.T) {
	report := CapabilityReportForModel(adaptermodel.ResolvedAlias{
		Alias:           "clyde-test-codex-1m-high",
		Backend:         adaptermodel.BackendCodex,
		ClaudeModel:     "configured-codex-model",
		Context:         1000000,
		ObservedContext: 333000,
	}, CapabilityMode{WebsocketEnabled: false})

	if report.AdvertisedContextWindow != 1000000 {
		t.Fatalf("advertised=%d want 1000000", report.AdvertisedContextWindow)
	}
	if report.ObservedContextWindow != 333000 {
		t.Fatalf("observed=%d want 333000", report.ObservedContextWindow)
	}
	if report.EffectiveSafeWindow != 299700 {
		t.Fatalf("effective=%d want 299700", report.EffectiveSafeWindow)
	}
}

func TestCapabilityReportForModelPreservesAdvertisedContextWhenWebsocketEnabled(t *testing.T) {
	report := CapabilityReportForModel(adaptermodel.ResolvedAlias{
		Alias:           "clyde-test-codex-1m-high",
		Backend:         adaptermodel.BackendCodex,
		ClaudeModel:     "configured-codex-model",
		Context:         1000000,
		ObservedContext: 333000,
	}, CapabilityMode{WebsocketEnabled: true})

	if report.ObservedContextWindow != 1000000 {
		t.Fatalf("observed=%d want 1000000", report.ObservedContextWindow)
	}
	if report.EffectiveSafeWindow != 900000 {
		t.Fatalf("effective=%d want 900000", report.EffectiveSafeWindow)
	}
}

func TestApplyCapabilityReportOverridesContextFields(t *testing.T) {
	entry := ApplyCapabilityReport(adapteropenai.ModelEntry{
		ID:            "clyde-gpt-5.4-1m-high",
		Context:       1000000,
		ContextWindow: 1000000,
		MaxModelLen:   1000000,
	}, CapabilityReport{
		AdvertisedContextWindow: 1000000,
		ObservedContextWindow:   272000,
		EffectiveSafeWindow:     244800,
	})

	if entry.Context != 272000 || entry.ContextWindow != 272000 || entry.MaxModelLen != 272000 {
		t.Fatalf("observed context fields = %+v", entry)
	}
	if entry.ContextTokenLimit != 244800 || entry.ContextTokenLimitCamel != 244800 || entry.ContextTokenLimitForMaxMode != 244800 || entry.ContextTokenLimitForMaxModeCamel != 244800 {
		t.Fatalf("effective safe fields = %+v", entry)
	}
}
