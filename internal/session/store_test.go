package session_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

var _ = Describe("FileStore", func() {
	var (
		tempDir   string
		clydeRoot string
		store     *session.FileStore
	)

	BeforeEach(func() {
		tempDir = GinkgoT().TempDir()
		clydeRoot = filepath.Join(tempDir, config.ClydeDir)
		err := util.EnsureDir(filepath.Join(clydeRoot, config.SessionsDir))
		Expect(err).NotTo(HaveOccurred())

		store = session.NewFileStore(clydeRoot)
	})

	Describe("Create and Get", func() {
		It("should create and retrieve a session", func() {
			s := session.NewSession("Test Session", "uuid-123")

			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			retrieved, err := store.Get("Test Session")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Name).To(Equal("Test Session"))
			Expect(retrieved.Metadata.ProviderSessionID()).To(Equal("uuid-123"))
		})

		It("should reject display names that cannot be matched exactly", func() {
			s := session.NewSession("line\nbreak", "uuid")
			err := store.Create(s)
			Expect(err).To(HaveOccurred())
		})

		It("should error if session already exists", func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			err = store.Create(s)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already exists"))
		})

		It("should resolve and search by display title", func() {
			s := session.NewSession("Merry Swan", "uuid-123")
			s.Metadata.DisplayTitle = "Merry Swan"

			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			resolved, err := store.Resolve("Merry Swan")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).ToNot(BeNil())
			Expect(resolved.Name).To(Equal("Merry Swan"))

			matches, err := store.Search("Merry Swan")
			Expect(err).NotTo(HaveOccurred())
			Expect(matches).To(HaveLen(1))
			Expect(matches[0].Name).To(Equal("Merry Swan"))
		})
	})

	Describe("Context field", func() {
		It("should preserve context through create/save/load cycle", func() {
			s := session.NewSession("ctx-session", "uuid-ctx")
			s.Metadata.Context = "working on ticket GH-123"

			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			retrieved, err := store.Get("ctx-session")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Metadata.Context).To(Equal("working on ticket GH-123"))
		})

		It("should omit context from JSON when empty", func() {
			s := session.NewSession("no-ctx-session", "uuid-no-ctx")

			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			retrieved, err := store.Get("no-ctx-session")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Metadata.Context).To(BeEmpty())
		})
	})

	Describe("Update", func() {
		It("should update session metadata", func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(10 * time.Millisecond)
			s.UpdateLastAccessed()
			err = store.Update(s)
			Expect(err).NotTo(HaveOccurred())

			retrieved, err := store.Get("test-session")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Metadata.LastAccessed).To(BeTemporally("~", s.Metadata.LastAccessed, time.Millisecond))
		})

		It("should error if session doesn't exist", func() {
			s := session.NewSession("nonexistent", "uuid")
			err := store.Update(s)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("Delete", func() {
		It("should delete a session", func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			err = store.Delete("test-session")
			Expect(err).NotTo(HaveOccurred())

			_, err = store.Get("test-session")
			Expect(err).To(HaveOccurred())
		})

		It("should error if session doesn't exist", func() {
			err := store.Delete("nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("Exists", func() {
		It("should return true if session exists", func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			Expect(store.Exists("test-session")).To(BeTrue())
		})

		It("should return false if session doesn't exist", func() {
			Expect(store.Exists("nonexistent")).To(BeFalse())
		})
	})

	Describe("List", func() {
		It("should list all sessions sorted by lastAccessed", func() {
			// Build each session AFTER its sleep. NewSession captures
			// time.Now() at construction, so declaring all three up
			// front and sleeping only between Create calls gives all
			// three the same timestamp and lets sort.Slice pick an
			// arbitrary order.
			s1 := session.NewSession("session-1", "uuid-1")
			Expect(store.Create(s1)).To(Succeed())

			time.Sleep(10 * time.Millisecond)
			s2 := session.NewSession("session-2", "uuid-2")
			Expect(store.Create(s2)).To(Succeed())

			time.Sleep(10 * time.Millisecond)
			s3 := session.NewSession("session-3", "uuid-3")
			Expect(store.Create(s3)).To(Succeed())

			sessions, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(sessions).To(HaveLen(3))

			// Should be sorted by lastAccessed (most recent first)
			Expect(sessions[0].Name).To(Equal("session-3"))
			Expect(sessions[1].Name).To(Equal("session-2"))
			Expect(sessions[2].Name).To(Equal("session-1"))
		})

		It("should return empty list if no sessions", func() {
			sessions, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(sessions).To(BeEmpty())
		})

		It("dedupes stale aliases that point at the same session id", func() {
			auto := &session.Session{
				Name: "clyde-dev-1a4837fd",
				Metadata: session.Metadata{
					Name:         "clyde-dev-1a4837fd",
					SessionID:    "uuid-shared",
					LastAccessed: time.Date(2026, 4, 21, 22, 10, 0, 0, time.UTC),
					Created:      time.Date(2026, 4, 20, 15, 25, 0, 0, time.UTC),
				},
			}
			human := &session.Session{
				Name: "unified-session-resolution",
				Metadata: session.Metadata{
					Name:         "unified-session-resolution",
					SessionID:    "uuid-shared",
					LastAccessed: time.Date(2026, 4, 21, 17, 17, 0, 0, time.UTC),
					Created:      time.Date(2026, 4, 20, 22, 26, 0, 0, time.UTC),
				},
			}

			Expect(store.Create(auto)).To(Succeed())
			Expect(store.Create(human)).To(Succeed())

			sessions, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(sessions).To(HaveLen(1))
			Expect(sessions[0].Name).To(Equal("unified-session-resolution"))
			Expect(sessions[0].Metadata.ProviderSessionID()).To(Equal("uuid-shared"))
		})

		It("keeps same direct session ids from different providers distinct", func() {
			claude := &session.Session{
				Name: "claude-session",
				Metadata: session.Metadata{
					Name:         "claude-session",
					Provider:     session.ProviderClaude,
					SessionID:    "shared-session-id",
					LastAccessed: time.Date(2026, 4, 21, 22, 10, 0, 0, time.UTC),
					Created:      time.Date(2026, 4, 20, 15, 25, 0, 0, time.UTC),
				},
			}
			codexLike := &session.Session{
				Name: "other-provider-session",
				Metadata: session.Metadata{
					Name:         "other-provider-session",
					Provider:     session.ProviderID("other-provider"),
					SessionID:    "shared-session-id",
					LastAccessed: time.Date(2026, 4, 21, 17, 17, 0, 0, time.UTC),
					Created:      time.Date(2026, 4, 20, 22, 26, 0, 0, time.UTC),
				},
			}

			Expect(store.Create(claude)).To(Succeed())
			Expect(store.Create(codexLike)).To(Succeed())

			sessions, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(sessions).To(HaveLen(2))
		})

		It("hydrates parent display names from parent ClydeUUID and exact session name", func() {
			parent := session.NewSession("Parent Current Name", "parent-provider-id")
			parent.Metadata.DisplayTitle = "Stale Provider Title"
			Expect(store.Create(parent)).To(Succeed())

			child := session.NewSession("Child Session", "child-provider-id")
			child.Metadata.IsForkedSession = true
			child.Metadata.ParentClydeUUID = parent.ClydeUUID()
			child.Metadata.ParentSession = "Old Parent Label"
			Expect(store.Create(child)).To(Succeed())

			sessions, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			var reloadedChild *session.Session
			for _, candidate := range sessions {
				if candidate.Name == "Child Session" {
					reloadedChild = candidate
				}
			}
			Expect(reloadedChild).ToNot(BeNil())
			Expect(reloadedChild.Metadata.ParentSession).To(Equal("Parent Current Name"))
		})
	})

	Describe("Resolve", func() {
		It("resolves an exact direct session id without UUID parsing", func() {
			s := session.NewSession("alpha-session", "provider-session-001")
			Expect(store.Create(s)).To(Succeed())

			resolved, err := store.Resolve("provider-session-001")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Name).To(Equal("alpha-session"))
		})

		It("resolves a previous direct session id without UUID parsing", func() {
			s := session.NewSession("history-session", "provider-session-current")
			s.Metadata.PreviousSessionIDs = []string{"provider-session-old"}
			Expect(store.Create(s)).To(Succeed())

			resolved, err := store.Resolve("provider-session-old")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Name).To(Equal("history-session"))
		})
	})

	Describe("ListForWorkspace", func() {
		It("matches canonical workspace roots", func() {
			workspace := filepath.Join(tempDir, "workspace")
			Expect(os.MkdirAll(workspace, 0o755)).To(Succeed())

			s := session.NewSession("workspace-session", "uuid-workspace")
			s.Metadata.WorkspaceRoot = filepath.Join(workspace, ".")
			Expect(store.Create(s)).To(Succeed())

			matches, err := store.ListForWorkspace(workspace)
			Expect(err).NotTo(HaveOccurred())
			Expect(matches).To(HaveLen(1))
			Expect(matches[0].Name).To(Equal("workspace-session"))
		})

		It("matches symlinked workspace roots", func() {
			workspace := filepath.Join(tempDir, "real-workspace")
			link := filepath.Join(tempDir, "workspace-link")
			Expect(os.MkdirAll(workspace, 0o755)).To(Succeed())
			if err := os.Symlink(workspace, link); err != nil {
				Skip("symlink unavailable: " + err.Error())
			}

			s := session.NewSession("symlink-session", "uuid-symlink")
			s.Metadata.WorkspaceRoot = link
			Expect(store.Create(s)).To(Succeed())

			matches, err := store.ListForWorkspace(workspace)
			Expect(err).NotTo(HaveOccurred())
			Expect(matches).To(HaveLen(1))
			Expect(matches[0].Name).To(Equal("symlink-session"))
		})
	})

	Describe("Settings operations", func() {
		BeforeEach(func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should save and load settings", func() {
			settings := &session.Settings{
				Model: "sonnet",
				Permissions: session.Permissions{
					Allow: []string{"Bash(git:*)"},
					Deny:  []string{"Read(./.env)"},
				},
			}

			err := store.SaveSettings("test-session", settings)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadSettings("test-session")
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Model).To(Equal("sonnet"))
			Expect(loaded.Permissions.Allow).To(ContainElement("Bash(git:*)"))
			Expect(loaded.Permissions.Deny).To(ContainElement("Read(./.env)"))
		})

		It("should return nil if settings don't exist", func() {
			loaded, err := store.LoadSettings("test-session")
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).To(BeNil())
		})
	})

	Describe("File existence checks", func() {
		It("should check if settings file exists", func() {
			s := session.NewSession("test-session", "uuid-123")
			err := store.Create(s)
			Expect(err).NotTo(HaveOccurred())

			sessionDir := config.GetSessionDir(clydeRoot, s.StorageKey())
			settingsPath := filepath.Join(sessionDir, "settings.json")

			Expect(util.FileExists(settingsPath)).To(BeFalse())

			settings := &session.Settings{Model: "sonnet"}
			err = store.SaveSettings("test-session", settings)
			Expect(err).NotTo(HaveOccurred())

			Expect(util.FileExists(settingsPath)).To(BeTrue())
		})
	})
})
