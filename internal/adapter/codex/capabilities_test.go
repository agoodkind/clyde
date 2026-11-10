package codex

import (
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

func TestCapabilityReportForModelUsesConfiguredTransportLimit(t *testing.T) {
	model := adaptermodel.ResolvedAlias{
		Alias:     "gpt-profiled",
		Backend:   adaptermodel.BackendCodex,
		WireModel: "gpt-profiled",
		Context:   372000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexHTTP:      272000,
			config.AdapterModelTransportCodexWebsocket: 350000,
		},
	}

	httpReport := CapabilityReportForModel(model, CapabilityMode{WebsocketEnabled: false})
	if httpReport.ObservedContextWindow != 272000 || httpReport.EffectiveSafeWindow != 272000 {
		t.Fatalf("HTTP report = %+v, want configured limit 272000", httpReport)
	}
	websocketReport := CapabilityReportForModel(model, CapabilityMode{WebsocketEnabled: true})
	if websocketReport.ObservedContextWindow != 350000 || websocketReport.EffectiveSafeWindow != 350000 {
		t.Fatalf("websocket report = %+v, want configured limit 350000", websocketReport)
	}
}

func TestCapabilityReportForModelUsesHTTPTransportLimit(t *testing.T) {
	report := CapabilityReportForModel(adaptermodel.ResolvedAlias{
		Alias:     "clyde-test-codex-1m-high",
		Backend:   adaptermodel.BackendCodex,
		WireModel: "configured-codex-model",
		Context:   1000000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexHTTP: 333000,
		},
	}, CapabilityMode{WebsocketEnabled: false})

	if report.AdvertisedContextWindow != 1000000 {
		t.Fatalf("advertised=%d want 1000000", report.AdvertisedContextWindow)
	}
	if report.ObservedContextWindow != 333000 {
		t.Fatalf("observed=%d want 333000", report.ObservedContextWindow)
	}
	if report.EffectiveSafeWindow != 333000 {
		t.Fatalf("effective=%d want 333000", report.EffectiveSafeWindow)
	}
}

func TestCapabilityReportForModelPreservesAdvertisedContextWhenWebsocketEnabled(t *testing.T) {
	report := CapabilityReportForModel(adaptermodel.ResolvedAlias{
		Alias:     "clyde-test-codex-1m-high",
		Backend:   adaptermodel.BackendCodex,
		WireModel: "configured-codex-model",
		Context:   1000000,
	}, CapabilityMode{WebsocketEnabled: true})

	if report.ObservedContextWindow != 1000000 {
		t.Fatalf("observed=%d want 1000000", report.ObservedContextWindow)
	}
	if report.EffectiveSafeWindow != 1000000 {
		t.Fatalf("effective=%d want 1000000", report.EffectiveSafeWindow)
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
