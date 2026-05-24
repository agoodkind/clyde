package config_test

import (
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/util"
)

var _ = Describe("Path helpers", func() {
	It("should construct sessions directory path", func() {
		root := "/project/.claude/clyde"
		path := config.GetSessionsDir(root)
		Expect(path).To(Equal("/project/.claude/clyde/sessions"))
	})

	It("should construct session directory path", func() {
		root := "/project/.claude/clyde"
		path := config.GetSessionDir(root, "my-session")
		Expect(path).To(Equal("/project/.claude/clyde/sessions/my-session"))
	})
})

var _ = Describe("XDG path helpers", func() {
	clydeUIDDir := func() string {
		return fmt.Sprintf("clyde-%d", os.Getuid())
	}

	It("resolves unset XDG roots from HOME and TMPDIR", func() {
		home := GinkgoT().TempDir()
		tmpDir := GinkgoT().TempDir()
		GinkgoT().Setenv("HOME", home)
		GinkgoT().Setenv("TMPDIR", tmpDir)
		GinkgoT().Setenv("XDG_CONFIG_HOME", "")
		GinkgoT().Setenv("XDG_CACHE_HOME", "")
		GinkgoT().Setenv("XDG_DATA_HOME", "")
		GinkgoT().Setenv("XDG_STATE_HOME", "")
		GinkgoT().Setenv("XDG_RUNTIME_DIR", "")

		Expect(config.GlobalConfigPath()).To(Equal(filepath.Join(home, ".config", "clyde", "config.toml")))
		Expect(config.GlobalCacheDir()).To(Equal(filepath.Join(home, ".cache", "clyde")))
		Expect(config.GlobalDataDir()).To(Equal(filepath.Join(home, ".local", "share", "clyde")))
		Expect(config.DefaultStateDir()).To(Equal(filepath.Join(home, ".local", "state", "clyde")))
		Expect(config.RuntimeDir()).To(Equal(filepath.Join(tmpDir, clydeUIDDir())))
	})

	It("resolves absolute XDG roots with Clyde app scoping", func() {
		root := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
		GinkgoT().Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
		GinkgoT().Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
		GinkgoT().Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
		GinkgoT().Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
		GinkgoT().Setenv("TMPDIR", filepath.Join(root, "tmp"))

		Expect(config.GlobalConfigPath()).To(Equal(filepath.Join(root, "config", "clyde", "config.toml")))
		Expect(config.GlobalCacheDir()).To(Equal(filepath.Join(root, "cache", "clyde")))
		Expect(config.GlobalDataDir()).To(Equal(filepath.Join(root, "data", "clyde")))
		Expect(config.DefaultStateDir()).To(Equal(filepath.Join(root, "state", "clyde")))
		Expect(config.RuntimeDir()).To(Equal(filepath.Join(root, "run", "clyde")))
	})

	It("home-expands and cleans XDG roots before app scoping", func() {
		home := GinkgoT().TempDir()
		GinkgoT().Setenv("HOME", home)
		GinkgoT().Setenv("XDG_CONFIG_HOME", "~/xdg/../config-root")
		GinkgoT().Setenv("XDG_CACHE_HOME", "~/xdg/../cache-root")
		GinkgoT().Setenv("XDG_DATA_HOME", "~/xdg/../data-root")
		GinkgoT().Setenv("XDG_STATE_HOME", "~/xdg/../state-root")
		GinkgoT().Setenv("XDG_RUNTIME_DIR", "~/xdg/../run-root")

		Expect(config.GlobalConfigPath()).To(Equal(filepath.Join(home, "config-root", "clyde", "config.toml")))
		Expect(config.GlobalCacheDir()).To(Equal(filepath.Join(home, "cache-root", "clyde")))
		Expect(config.GlobalDataDir()).To(Equal(filepath.Join(home, "data-root", "clyde")))
		Expect(config.DefaultStateDir()).To(Equal(filepath.Join(home, "state-root", "clyde")))
		Expect(config.RuntimeDir()).To(Equal(filepath.Join(home, "run-root", "clyde")))
	})

	It("falls back from runtime XDG to TMPDIR, then os.TempDir", func() {
		tmpDir := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_RUNTIME_DIR", "")
		GinkgoT().Setenv("TMPDIR", filepath.Join(tmpDir, "tmpdir"))
		Expect(config.RuntimeDir()).To(Equal(filepath.Join(tmpDir, "tmpdir", clydeUIDDir())))

		GinkgoT().Setenv("TMPDIR", "")
		Expect(config.RuntimeDir()).To(Equal(filepath.Join(os.TempDir(), clydeUIDDir())))
	})
})

var _ = Describe("ProjectRootFromPath", func() {
	var tempDir string

	BeforeEach(func() {
		tempDir = GinkgoT().TempDir()
	})

	It("should find project root with .claude directory", func() {
		claudeDir := filepath.Join(tempDir, ".claude")
		err := util.EnsureDir(claudeDir)
		Expect(err).NotTo(HaveOccurred())

		root := config.ProjectRootFromPath(tempDir)
		Expect(root).To(Equal(tempDir))
	})

	It("should find project root from nested path", func() {
		claudeDir := filepath.Join(tempDir, ".claude")
		err := util.EnsureDir(claudeDir)
		Expect(err).NotTo(HaveOccurred())

		nestedDir := filepath.Join(tempDir, "nested", "deep")
		err = util.EnsureDir(nestedDir)
		Expect(err).NotTo(HaveOccurred())

		root := config.ProjectRootFromPath(nestedDir)
		Expect(root).To(Equal(tempDir))
	})

	It("should return start path if no .claude directory found", func() {
		root := config.ProjectRootFromPath(tempDir)
		Expect(root).To(Equal(tempDir))
	})

	It("should not walk above $HOME to find .claude", func() {
		// Use tempDir as a fake $HOME with .claude/ in it
		GinkgoT().Setenv("HOME", tempDir)

		claudeDir := filepath.Join(tempDir, ".claude")
		err := util.EnsureDir(claudeDir)
		Expect(err).NotTo(HaveOccurred())

		subDir := filepath.Join(tempDir, "projects", "myapp")
		err = util.EnsureDir(subDir)
		Expect(err).NotTo(HaveOccurred())

		// ProjectRootFromPath should NOT treat $HOME as the project root
		// even though ~/.claude/ exists
		root := config.ProjectRootFromPath(subDir)
		Expect(root).To(Equal(subDir))
		Expect(root).NotTo(Equal(tempDir))
	})
})
