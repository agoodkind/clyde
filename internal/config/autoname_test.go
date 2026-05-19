package config_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("AutoName config", func() {
	var origXDG string

	BeforeEach(func() {
		origXDG = os.Getenv("XDG_CONFIG_HOME")
	})

	AfterEach(func() {
		if origXDG == "" {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		} else {
			_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		}
	})

	writeConfig := func(body string) {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(body), 0o644)).To(Succeed())
	}

	It("applies defaults when the [autoname] block is absent", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.AutoName.IsEnabled()).To(BeTrue())
		Expect(cfg.AutoName.Provider).To(Equal(""))
		Expect(cfg.AutoName.MaxCallsPerHour).To(Equal(6))
		Expect(cfg.AutoName.Cooldown.AsDuration()).To(Equal(30 * time.Minute))
		Expect(cfg.AutoName.MinUserMessages).To(Equal(3))
		Expect(cfg.AutoName.Redact.MinDigitRunForRedact).To(Equal(7))
		Expect(cfg.AutoName.Redact.StripEmailsOrDefault()).To(BeTrue())
		Expect(cfg.AutoName.Redact.StripPathsOrDefault()).To(BeTrue())
		Expect(cfg.AutoName.Redact.StripKeyPrefixesOrDefault()).To(BeTrue())
	})

	It("returns the matching struct when the block is fully populated", func() {
		writeConfig(`
[autoname]
enabled = false
provider = "summary-route"
max_calls_per_hour = 12
cooldown = "15m"
min_user_messages = 5

[autoname.redact]
min_digit_run_for_redact = 10
strip_emails = false
strip_paths = false
strip_key_prefixes = false
`)
		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.AutoName.IsEnabled()).To(BeFalse())
		Expect(cfg.AutoName.Provider).To(Equal("summary-route"))
		Expect(cfg.AutoName.MaxCallsPerHour).To(Equal(12))
		Expect(cfg.AutoName.Cooldown.AsDuration()).To(Equal(15 * time.Minute))
		Expect(cfg.AutoName.MinUserMessages).To(Equal(5))
		Expect(cfg.AutoName.Redact.MinDigitRunForRedact).To(Equal(10))
		Expect(cfg.AutoName.Redact.StripEmailsOrDefault()).To(BeFalse())
		Expect(cfg.AutoName.Redact.StripPathsOrDefault()).To(BeFalse())
		Expect(cfg.AutoName.Redact.StripKeyPrefixesOrDefault()).To(BeFalse())
	})

	It("merges defaults when only enabled is set in a partial block", func() {
		writeConfig(`
[autoname]
enabled = false
`)
		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.AutoName.IsEnabled()).To(BeFalse())
		Expect(cfg.AutoName.Provider).To(Equal(""))
		Expect(cfg.AutoName.MaxCallsPerHour).To(Equal(6))
		Expect(cfg.AutoName.Cooldown.AsDuration()).To(Equal(30 * time.Minute))
		Expect(cfg.AutoName.MinUserMessages).To(Equal(3))
		Expect(cfg.AutoName.Redact.MinDigitRunForRedact).To(Equal(7))
		Expect(cfg.AutoName.Redact.StripEmailsOrDefault()).To(BeTrue())
		Expect(cfg.AutoName.Redact.StripPathsOrDefault()).To(BeTrue())
		Expect(cfg.AutoName.Redact.StripKeyPrefixesOrDefault()).To(BeTrue())
	})

	It("parses the nested [autoname.redact] block in isolation", func() {
		writeConfig(`
[autoname.redact]
min_digit_run_for_redact = 9
strip_emails = false
`)
		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.AutoName.IsEnabled()).To(BeTrue())
		Expect(cfg.AutoName.MaxCallsPerHour).To(Equal(6))
		Expect(cfg.AutoName.Cooldown.AsDuration()).To(Equal(30 * time.Minute))
		Expect(cfg.AutoName.MinUserMessages).To(Equal(3))
		Expect(cfg.AutoName.Redact.MinDigitRunForRedact).To(Equal(9))
		Expect(cfg.AutoName.Redact.StripEmailsOrDefault()).To(BeFalse())
		Expect(cfg.AutoName.Redact.StripPathsOrDefault()).To(BeTrue())
		Expect(cfg.AutoName.Redact.StripKeyPrefixesOrDefault()).To(BeTrue())
	})
})
