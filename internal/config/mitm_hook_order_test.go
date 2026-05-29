package config_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("MITM hook declaration order", func() {
	var configDir string

	writeConfig := func(body string) {
		path := filepath.Join(configDir, "clyde", "config.toml")
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
	}

	BeforeEach(func() {
		configDir = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_STATE_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("XDG_CONFIG_HOME", configDir)
	})

	It("preserves the TOML declaration order of [[mitm.hook]] entries", func() {
		// Declare the hooks out of alphabetical order so a stable sort or a
		// map-backed decode would visibly reorder them; the slice must come
		// back in the exact order the blocks appear in the file because
		// matchHookRule evaluates hooks in declaration order and the first
		// host/method/path match wins.
		writeConfig(`[mitm]

[[mitm.hook]]
name = "zeta"
match_host = "z.example.com"
command = "/bin/true"

[[mitm.hook]]
name = "alpha"
match_host = "a.example.com"
command = "/bin/true"

[[mitm.hook]]
name = "mike"
match_host = "m.example.com"
command = "/bin/true"

[[mitm.hook]]
name = "bravo"
match_host = "b.example.com"
command = "/bin/true"
`)

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())

		names := make([]string, 0, len(cfg.MITM.Hooks))
		for _, hook := range cfg.MITM.Hooks {
			names = append(names, hook.Name)
		}
		Expect(names).To(Equal([]string{"zeta", "alpha", "mike", "bravo"}))
	})
})
