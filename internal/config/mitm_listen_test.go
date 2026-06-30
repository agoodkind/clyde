package config_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("MITM listener and CA config schema", func() {
	var (
		stateDir  string
		configDir string
	)

	writeConfig := func(body string) {
		path := filepath.Join(configDir, "clyde", "config.toml")
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
	}

	BeforeEach(func() {
		stateDir = GinkgoT().TempDir()
		configDir = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_STATE_HOME", stateDir)
		GinkgoT().Setenv("XDG_CONFIG_HOME", configDir)
	})

	It("synthesizes a default listener when [mitm.app]/[mitm.cli] and [mitm.ca] are absent", func() {
		writeConfig("[mitm]\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listeners).To(HaveLen(1))
		Expect(cfg.MITM.Listeners["default"].ID).To(Equal("default"))
		Expect(cfg.MITM.Listeners["default"].Host).To(Equal("[::1]"))
		Expect(cfg.MITM.Listeners["default"].Port).To(Equal(48723))

		expectedCertDir := filepath.Join(stateDir, "clyde", "mitm", "ca")
		Expect(cfg.MITM.CA.CertPath).To(Equal(filepath.Join(expectedCertDir, "clyde-mitm-ca.crt")))
		Expect(cfg.MITM.CA.KeyPath).To(Equal(filepath.Join(expectedCertDir, "clyde-mitm-ca.key")))
		Expect(filepath.IsAbs(cfg.MITM.CA.CertPath)).To(BeTrue())
		Expect(filepath.IsAbs(cfg.MITM.CA.KeyPath)).To(BeTrue())
	})

	It("flattens cli and app groups into group-qualified listener ids", func() {
		writeConfig("[mitm.cli.claude-code]\nhost = \"localhost\"\nport = 48723\n\n[mitm.cli.codex]\nhost = \"localhost\"\nport = 48724\n\n[mitm.app.codex]\nhost = \"localhost\"\nport = 48727\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listeners).To(HaveLen(3))
		Expect(cfg.MITM.Listeners["cli.claude-code"].ID).To(Equal("cli.claude-code"))
		Expect(cfg.MITM.Listeners["cli.claude-code"].Port).To(Equal(48723))
		Expect(cfg.MITM.Listeners["cli.codex"].Port).To(Equal(48724))
		Expect(cfg.MITM.Listeners["app.codex"].Port).To(Equal(48727))
	})

	It("rejects a listener with a negative port", func() {
		writeConfig("[mitm.app.cursor]\nport = -1\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.app.cursor"))
	})

	It("rejects a listener with a port above 65535", func() {
		writeConfig("[mitm.cli.codex]\nport = 65536\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.cli.codex"))
	})

	It("rejects a listener with an empty name table key", func() {
		writeConfig("[mitm.app.\"\"]\nport = 48723\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.app"))
	})

	It("substitutes default host when a listener host is empty", func() {
		writeConfig("[mitm.app.cursor]\nhost = \"\"\nport = 8080\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listeners["app.cursor"].Host).To(Equal("[::1]"))
		Expect(cfg.MITM.Listeners["app.cursor"].Port).To(Equal(8080))
	})

	It("rejects a non-absolute ca.cert_path after expansion", func() {
		writeConfig("[mitm.ca]\ncert_path = \"relative/path/ca.crt\"\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.ca.cert_path"))
		Expect(err.Error()).To(ContainSubstring("absolute"))
	})

	It("rejects a non-absolute ca.key_path after expansion", func() {
		writeConfig("[mitm.ca]\nkey_path = \"relative/path/ca.key\"\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.ca.key_path"))
		Expect(err.Error()).To(ContainSubstring("absolute"))
	})
})
