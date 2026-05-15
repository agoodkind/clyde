package config_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

func loadRetryConfigForTest(snippet string) (*config.Config, error) {
	tmpDir := GinkgoT().TempDir()
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	DeferCleanup(func() {
		if origXDG == "" {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		} else {
			_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		}
	})
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	globalDir := filepath.Join(tmpDir, "clyde")
	Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(snippet), 0o644)).To(Succeed())
	return config.LoadGlobalOrDefault()
}

var _ = Describe("Adapter retry config", func() {
	It("leaves retry policies empty when config supplies none", func() {
		cfg, err := loadRetryConfigForTest("")
		Expect(err).NotTo(HaveOccurred())
		// Provider builtins are appended by each provider package at
		// construction, not injected into the generic config layer.
		Expect(cfg.Adapter.Retry.Policies).To(BeEmpty())
	})

	It("parses and normalizes configured policies", func() {
		cfg, err := loadRetryConfigForTest(`
[[adapter.retry.policies]]
name = "custom"
max_attempts = 2
initial_delay = "10ms"
max_delay = "50ms"
multiplier = 1.5
jitter_fraction = 0.25
retry_when_response_started = true

[adapter.retry.policies.match]
backends = [" codex "]
operations = ["codex.responses.websocket.generate"]
statuses = [503]
error_classes = ["scanner_error"]
error_codes = ["overloaded"]
message_substrings = ["overloaded"]
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.Retry.Policies).To(HaveLen(1))
		policy := cfg.Adapter.Retry.Policies[0]
		Expect(policy.Name).To(Equal("custom"))
		Expect(policy.Enabled).NotTo(BeNil())
		Expect(*policy.Enabled).To(BeTrue())
		Expect(policy.InitialDelay.Duration()).To(Equal(10 * time.Millisecond))
		Expect(policy.MaxDelay.Duration()).To(Equal(50 * time.Millisecond))
		Expect(policy.Match.Backends).To(Equal([]string{"codex"}))
	})

	It("rejects invalid retry settings", func() {
		_, err := loadRetryConfigForTest(`
[[adapter.retry.policies]]
name = "bad"
max_attempts = 0
`)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.retry.policies.bad.max_attempts"))
	})
})
