package config_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("MITM listen and CA config schema", func() {
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

	It("applies defaults when [mitm.listen] and [mitm.ca] are absent", func() {
		writeConfig("[mitm]\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listen.Host).To(Equal("[::1]"))
		Expect(cfg.MITM.Listen.Port).To(Equal(48723))

		expectedCertDir := filepath.Join(stateDir, "clyde", "mitm", "ca")
		Expect(cfg.MITM.CA.CertPath).To(Equal(filepath.Join(expectedCertDir, "clyde-mitm-ca.crt")))
		Expect(cfg.MITM.CA.KeyPath).To(Equal(filepath.Join(expectedCertDir, "clyde-mitm-ca.key")))
		Expect(filepath.IsAbs(cfg.MITM.CA.CertPath)).To(BeTrue())
		Expect(filepath.IsAbs(cfg.MITM.CA.KeyPath)).To(BeTrue())
	})

	It("substitutes default port when listen.port is zero", func() {
		writeConfig("[mitm.listen]\nport = 0\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listen.Port).To(Equal(48723))
	})

	It("rejects negative listen.port", func() {
		writeConfig("[mitm.listen]\nport = -1\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.listen.port"))
		Expect(err.Error()).To(ContainSubstring("-1"))
	})

	It("rejects listen.port above 65535", func() {
		writeConfig("[mitm.listen]\nport = 65536\n")

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.listen.port"))
		Expect(err.Error()).To(ContainSubstring("65536"))
	})

	It("substitutes default host when listen.host is empty", func() {
		writeConfig("[mitm.listen]\nhost = \"\"\nport = 8080\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listen.Host).To(Equal("[::1]"))
		Expect(cfg.MITM.Listen.Port).To(Equal(8080))
	})

	It("trims whitespace-only host and substitutes the default", func() {
		writeConfig("[mitm.listen]\nhost = \"   \"\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listen.Host).To(Equal("[::1]"))
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

	It("ignores CLYDE_MITM_HOST, CLYDE_MITM_PORT, CLYDE_MITM_CERT_PATH, CLYDE_MITM_KEY_PATH overrides", func() {
		GinkgoT().Setenv("CLYDE_MITM_HOST", "203.0.113.1")
		GinkgoT().Setenv("CLYDE_MITM_PORT", "9999")
		GinkgoT().Setenv("CLYDE_MITM_CERT_PATH", "/tmp/should-not-be-used.crt")
		GinkgoT().Setenv("CLYDE_MITM_KEY_PATH", "/tmp/should-not-be-used.key")
		writeConfig("[mitm.listen]\nport = 0\n")

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Listen.Port).To(Equal(48723))
		Expect(cfg.MITM.Listen.Host).To(Equal("[::1]"))
		Expect(cfg.MITM.CA.CertPath).NotTo(Equal("/tmp/should-not-be-used.crt"))
		Expect(cfg.MITM.CA.KeyPath).NotTo(Equal("/tmp/should-not-be-used.key"))
		Expect(strings.HasPrefix(cfg.MITM.CA.CertPath, stateDir)).To(BeTrue())
		Expect(strings.HasPrefix(cfg.MITM.CA.KeyPath, stateDir)).To(BeTrue())
	})
})
