package config_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
	"goodkind.io/clyde/internal/config"
)

// configWithReasoningTOML writes the supplied snippet under
// $XDG_CONFIG_HOME/clyde/config.toml and returns the loaded config.
func configWithReasoningTOML(snippet string) (*config.Config, error) {
	tmpDir := GinkgoT().TempDir()
	GinkgoT().Setenv("XDG_CONFIG_HOME", tmpDir)
	globalDir := filepath.Join(tmpDir, "clyde")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(snippet), 0o644); err != nil {
		return nil, err
	}
	return config.LoadGlobalOrDefault()
}

var _ = Describe("Adapter reasoning config", func() {
	Describe("AdapterAnthropicReasoning.ResolvedInboundThinking", func() {
		DescribeTable("resolves the configured value with the documented default applied when empty",
			func(input anthmode.InboundThinking, expected anthmode.InboundThinking) {
				r := config.AdapterAnthropicReasoning{InboundThinking: input}
				Expect(r.ResolvedInboundThinking()).To(Equal(expected))
			},
			Entry("empty -> native_thinking_block default", anthmode.InboundThinking(""), anthmode.InboundThinkingNative),
			Entry("native_thinking_block passes through", anthmode.InboundThinkingNative, anthmode.InboundThinkingNative),
			Entry("drop passes through", anthmode.InboundThinkingDrop, anthmode.InboundThinkingDrop),
			Entry("plain_text_concat passes through", anthmode.InboundThinkingPlainText, anthmode.InboundThinkingPlainText),
			Entry("passthrough passes through", anthmode.InboundThinkingPassthrough, anthmode.InboundThinkingPassthrough),
		)
	})

	Describe("AdapterCodexReasoning.ResolvedRoundTripSummary", func() {
		DescribeTable("resolves the configured value with the documented default applied when empty",
			func(input config.CodexRoundTripSummary, expected config.CodexRoundTripSummary) {
				r := config.AdapterCodexReasoning{RoundTripSummary: input}
				Expect(r.ResolvedRoundTripSummary()).To(Equal(expected))
			},
			Entry("empty -> native_summary_field default", config.CodexRoundTripSummary(""), config.CodexRoundTripSummaryNative),
			Entry("native_summary_field passes through", config.CodexRoundTripSummaryNative, config.CodexRoundTripSummaryNative),
			Entry("drop passes through", config.CodexRoundTripSummaryDrop, config.CodexRoundTripSummaryDrop),
			Entry("plain_text_concat passes through", config.CodexRoundTripSummaryPlainText, config.CodexRoundTripSummaryPlainText),
		)
	})

	Describe("AdapterCodexReasoning.ResolvedRoundTripEncrypted", func() {
		DescribeTable("resolves the configured value with the documented default applied when empty",
			func(input config.CodexRoundTripEncrypted, expected config.CodexRoundTripEncrypted) {
				r := config.AdapterCodexReasoning{RoundTripEncrypted: input}
				Expect(r.ResolvedRoundTripEncrypted()).To(Equal(expected))
			},
			Entry("empty -> round_trip default", config.CodexRoundTripEncrypted(""), config.CodexRoundTripEncryptedRoundTrip),
			Entry("round_trip passes through", config.CodexRoundTripEncryptedRoundTrip, config.CodexRoundTripEncryptedRoundTrip),
			Entry("drop passes through", config.CodexRoundTripEncryptedDrop, config.CodexRoundTripEncryptedDrop),
		)
	})

	Describe("load-time validation", func() {
		It("accepts a fully populated reasoning block", func() {
			cfg, err := configWithReasoningTOML(`
[adapter.anthropic.reasoning]
inbound_thinking = "drop"

[adapter.codex.reasoning]
round_trip_summary = "plain_text_concat"
round_trip_encrypted = "drop"
`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Adapter.Anthropic.Reasoning.ResolvedInboundThinking()).To(Equal(anthmode.InboundThinkingDrop))
			Expect(cfg.Adapter.Codex.Reasoning.ResolvedRoundTripSummary()).To(Equal(config.CodexRoundTripSummaryPlainText))
			Expect(cfg.Adapter.Codex.Reasoning.ResolvedRoundTripEncrypted()).To(Equal(config.CodexRoundTripEncryptedDrop))
		})

		It("rejects an unknown anthropic inbound_thinking value", func() {
			_, err := configWithReasoningTOML(`
[adapter.anthropic.reasoning]
inbound_thinking = "made_up_strategy"
`)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("adapter.anthropic.reasoning.inbound_thinking"))
		})

		It("rejects an unknown codex round_trip_summary value", func() {
			_, err := configWithReasoningTOML(`
[adapter.codex.reasoning]
round_trip_summary = "made_up_strategy"
`)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("adapter.codex.reasoning.round_trip_summary"))
		})

		It("rejects an unknown codex round_trip_encrypted value", func() {
			_, err := configWithReasoningTOML(`
[adapter.codex.reasoning]
round_trip_encrypted = "maybe"
`)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("adapter.codex.reasoning.round_trip_encrypted"))
		})
	})
})
