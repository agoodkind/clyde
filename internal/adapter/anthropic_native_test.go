package adapter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

func TestApplyAnthropicNativeResolutionSetsResolvedEffort(t *testing.T) {
	tests := []struct {
		name         string
		outputConfig *anthropic.OutputConfig
		resolved     adapterresolver.Effort
		wire         adapterresolver.Effort
		want         string
	}{
		{name: "omitted output config", resolved: "medium", want: "medium"},
		{name: "empty output config", outputConfig: &anthropic.OutputConfig{}, resolved: "medium", want: "medium"},
		{name: "populated exact effort", outputConfig: &anthropic.OutputConfig{Effort: "high"}, resolved: "high", want: "high"},
		{name: "mapped exact effort", outputConfig: &anthropic.OutputConfig{Effort: "ultra"}, resolved: "ultra", wire: "max", want: "max"},
		{name: "populated wildcard effort", outputConfig: &anthropic.OutputConfig{Effort: "future-tier"}, resolved: "future-tier", want: "future-tier"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := anthropic.Request{
				Model:        "claude-test",
				MaxTokens:    8000,
				OutputConfig: test.outputConfig,
			}
			resolved := adapterresolver.ResolvedRequest{
				Provider:   adaptermodel.BackendAnthropic,
				Model:      "claude-test",
				Effort:     test.resolved,
				WireEffort: test.wire,
			}

			applyAnthropicNativeResolution(&req, &resolved)

			if req.OutputConfig == nil || req.OutputConfig.Effort != test.want {
				t.Fatalf("OutputConfig = %+v, want effort %q", req.OutputConfig, test.want)
			}
		})
	}
}

func TestApplyAnthropicNativeResolutionUsesConfiguredThinkingBudget(t *testing.T) {
	req := anthropic.Request{
		Model:     "claude-test",
		MaxTokens: 9000,
		Thinking:  &anthropic.Thinking{Type: "adaptive"},
	}
	resolved := adapterresolver.ResolvedRequest{
		Provider:             adaptermodel.BackendAnthropic,
		Model:                "claude-test",
		Thinking:             adaptermodel.ThinkingEnabled,
		ThinkingBudgetTokens: 7000,
		MaxOutputTokens:      10000,
	}

	applyAnthropicNativeResolution(&req, &resolved)

	if req.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 7000 {
		t.Fatalf("Thinking = %+v, want enabled with budget 7000", req.Thinking)
	}
	if req.MaxTokens != 9000 {
		t.Fatalf("MaxTokens = %d, want caller value 9000", req.MaxTokens)
	}
}

func TestApplyAnthropicNativeResolutionCapsTokensAfterThinkingBudget(t *testing.T) {
	req := anthropic.Request{
		Model:     "claude-test",
		MaxTokens: 16000,
		Thinking:  &anthropic.Thinking{Type: "adaptive"},
	}
	resolved := adapterresolver.ResolvedRequest{
		Provider:             adaptermodel.BackendAnthropic,
		Model:                "claude-test",
		Thinking:             adaptermodel.ThinkingEnabled,
		ThinkingBudgetTokens: 16000,
		MaxOutputTokens:      16000,
	}

	applyAnthropicNativeResolution(&req, &resolved)

	if req.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 16000 {
		t.Fatalf("Thinking = %+v, want enabled with budget 16000", req.Thinking)
	}
	if req.MaxTokens != 16000 {
		t.Fatalf("MaxTokens = %d, want profile cap 16000", req.MaxTokens)
	}
}

func TestApplyAnthropicNativeResolutionPreservesWildcardThinking(t *testing.T) {
	want := &anthropic.Thinking{Type: "enabled", BudgetTokens: 3210}
	req := anthropic.Request{Model: "claude-future", MaxTokens: 4000, Thinking: want}
	resolved := adapterresolver.ResolvedRequest{
		Provider: adaptermodel.BackendAnthropic,
		Model:    "claude-future",
	}

	applyAnthropicNativeResolution(&req, &resolved)

	if req.Thinking != want {
		t.Fatalf("Thinking = %+v, want caller value preserved", req.Thinking)
	}
}

func TestApplyAnthropicNativeResolutionPrependsInstructions(t *testing.T) {
	tests := []struct {
		name       string
		system     string
		blocks     []anthropic.SystemBlock
		wantSystem string
		wantBlocks []string
	}{
		{
			name:       "plain system",
			system:     "caller system",
			wantSystem: "model instructions\n\ncaller system",
		},
		{
			name:       "empty system",
			wantSystem: "model instructions",
		},
		{
			name: "system blocks",
			blocks: []anthropic.SystemBlock{
				{Type: "text", Text: "caller system"},
			},
			wantBlocks: []string{"model instructions", "caller system"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := anthropic.Request{
				Model:        "claude-test",
				System:       test.system,
				SystemBlocks: test.blocks,
				MaxTokens:    8000,
			}
			resolved := adapterresolver.ResolvedRequest{
				Provider:     adaptermodel.BackendAnthropic,
				Model:        "claude-test",
				Instructions: "model instructions",
			}

			applyAnthropicNativeResolution(&req, &resolved)

			if req.System != test.wantSystem {
				t.Fatalf("System = %q, want %q", req.System, test.wantSystem)
			}
			if len(req.SystemBlocks) != len(test.wantBlocks) {
				t.Fatalf("SystemBlocks = %+v, want text %v", req.SystemBlocks, test.wantBlocks)
			}
			for index, want := range test.wantBlocks {
				if req.SystemBlocks[index].Text != want {
					t.Fatalf("SystemBlocks[%d].Text = %q, want %q", index, req.SystemBlocks[index].Text, want)
				}
			}
		})
	}
}

func TestNativeAnthropicJSONWriterCaptureUsesStatusOK(t *testing.T) {
	writer := newNativeAnthropicJSONWriter()
	writer.status = http.StatusTeapot
	if err := writer.capture(http.Header{"Content-Type": {"application/json"}}, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("capture: %v", err)
	}

	response := httptest.NewRecorder()
	writer.writeTo(response)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}
