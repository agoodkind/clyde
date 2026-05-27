package claudepath_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

var _ = Describe("claudepath", func() {
	Describe("ProjectDir", func() {
		It("encodes the project path correctly", func() {
			Expect(claudepath.ProjectDir("/home/user/project/.claude/clyde")).To(Equal("-home-user-project"))
		})

		It("replaces dots with dashes", func() {
			Expect(claudepath.ProjectDir("/home/user/my.project/.claude/clyde")).To(Equal("-home-user-my-project"))
		})

		It("handles deeply nested paths", func() {
			Expect(claudepath.ProjectDir("/home/user/projects/foo/bar/.claude/clyde")).To(Equal("-home-user-projects-foo-bar"))
		})
	})

	Describe("TranscriptPath", func() {
		It("returns the canonical transcript path under <home>/.claude/projects", func() {
			home := "/home/user"
			clydeRoot := "/home/user/project/.claude/clyde"
			sessionID := "550e8400-e29b-41d4-a716-446655440000"
			expected := filepath.Join(home, ".claude", "projects", "-home-user-project", sessionID+".jsonl")
			Expect(claudepath.TranscriptPath(home, clydeRoot, sessionID)).To(Equal(expected))
		})
	})

	Describe("TranscriptPathForWorkspace", func() {
		It("returns the canonical transcript path from workspace root plus uuid", func() {
			home := "/home/user"
			workspaceRoot := "/Users/test/Sites/configs"
			sessionID := "0630f096-a323-42e3-bbb1-03730cfe24b0"
			expected := filepath.Join(home, ".claude", "projects", "-Users-test-Sites-configs", sessionID+".jsonl")
			Expect(claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)).To(Equal(expected))
		})
	})

	Describe("CanonicalProjectsRoot", func() {
		It("returns <home>/.claude/projects", func() {
			Expect(claudepath.CanonicalProjectsRoot("/home/user")).To(Equal(filepath.Join("/home/user", ".claude", "projects")))
		})
	})

	Describe("EncodeWorkspaceRoot", func() {
		It("replaces slashes and dots with dashes", func() {
			Expect(claudepath.EncodeWorkspaceRoot("/Users/agoodkind/.dotfiles")).To(Equal("-Users-agoodkind--dotfiles"))
		})
	})
})
