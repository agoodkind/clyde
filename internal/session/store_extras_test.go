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

// providerNameStub satisfies session.ProviderSessionName for SyncDiscoveryResults
// fixtures. It mirrors the contract used by tier4TestName in
// resolve_tier4_test.go but lives in the session_test package so external tests
// can construct DiscoveryResult values with a non-nil NameContract.
type providerNameStub struct {
	value string
}

func (n providerNameStub) GetName() string {
	return n.value
}

func (n providerNameStub) Rename(_ string, taken map[string]bool) string {
	candidate := session.UniqueDisplayName(n.value, taken)
	if candidate == "" || session.ValidateDisplayName(candidate) != nil {
		return ""
	}
	return candidate
}

var _ = Describe("FileStore additional contracts", func() {
	var (
		tempDir   string
		clydeRoot string
		store     *session.FileStore
	)

	BeforeEach(func() {
		tempDir = GinkgoT().TempDir()
		clydeRoot = filepath.Join(tempDir, config.ClydeDir)
		Expect(util.EnsureDir(filepath.Join(clydeRoot, config.SessionsDir))).To(Succeed())
		store = session.NewFileStore(clydeRoot)
	})

	Describe("Rename", func() {
		It("rejects an invalid old name", func() {
			err := store.Rename("bad\nname", "Anything Goes")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid old name"))
		})

		It("rejects an invalid new name", func() {
			s := session.NewSession("Original Name", "uuid-rename-1")
			Expect(store.Create(s)).To(Succeed())

			err := store.Rename("Original Name", "bad\nname")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid new name"))

			// The original session must remain reachable under its original name
			// when the rename was rejected at the validation boundary.
			retrieved, getErr := store.Get("Original Name")
			Expect(getErr).NotTo(HaveOccurred())
			Expect(retrieved.Name).To(Equal("Original Name"))
		})

		It("returns not found when the old session does not exist", func() {
			err := store.Rename("Missing Session", "New Name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("returns already exists when the target name collides", func() {
			s1 := session.NewSession("Source Name", "uuid-rename-src")
			Expect(store.Create(s1)).To(Succeed())
			s2 := session.NewSession("Target Name", "uuid-rename-tgt")
			Expect(store.Create(s2)).To(Succeed())

			err := store.Rename("Source Name", "Target Name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already exists"))

			// Both sessions must still resolve under their original names.
			src, err := store.Get("Source Name")
			Expect(err).NotTo(HaveOccurred())
			Expect(src.Metadata.ProviderSessionID()).To(Equal("uuid-rename-src"))

			tgt, err := store.Get("Target Name")
			Expect(err).NotTo(HaveOccurred())
			Expect(tgt.Metadata.ProviderSessionID()).To(Equal("uuid-rename-tgt"))
		})

		It("renames a session and updates metadata when both names are valid", func() {
			s := session.NewSession("Old Name", "uuid-rename-happy")
			Expect(store.Create(s)).To(Succeed())

			Expect(store.Rename("Old Name", "Brand New Name")).To(Succeed())

			_, err := store.Get("Old Name")
			Expect(err).To(HaveOccurred())

			renamed, err := store.Get("Brand New Name")
			Expect(err).NotTo(HaveOccurred())
			Expect(renamed.Name).To(Equal("Brand New Name"))
			Expect(renamed.Metadata.Name).To(Equal("Brand New Name"))
			Expect(renamed.Metadata.Title).To(Equal("Brand New Name"))
			Expect(renamed.Metadata.ProviderSessionID()).To(Equal("uuid-rename-happy"))
		})
	})

	Describe("Title metadata migration", func() {
		It("rewrites legacy displayTitle keys to title", func() {
			sessionDir := filepath.Join(clydeRoot, config.SessionsDir, "legacy-title")
			Expect(util.EnsureDir(sessionDir)).To(Succeed())
			metadataPath := filepath.Join(sessionDir, "metadata.json")
			body := []byte(`{"name":"Legacy Title","sessionId":"uuid-title","displayTitle":"Provider Title"}`)
			Expect(os.WriteFile(metadataPath, body, 0o644)).To(Succeed())

			result, err := session.MigrateTitleMetadata(clydeRoot)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Scanned).To(Equal(1))
			Expect(result.Migrated).To(Equal(1))
			Expect(result.Failed).To(Equal(0))

			reloaded, err := session.NewFileStore(clydeRoot).Get("Legacy Title")
			Expect(err).ToNot(HaveOccurred())
			Expect(reloaded.Metadata.Title).To(Equal("Provider Title"))

			content, err := os.ReadFile(metadataPath)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(content)).To(ContainSubstring(`"title"`))
			Expect(string(content)).ToNot(ContainSubstring(`"displayTitle"`))
		})
	})

	Describe("Update directory migration", func() {
		It("migrates a legacy directory to a ClydeUUID-keyed directory when metadata lacks a ClydeUUID", func() {
			// Pre-create a legacy session storage directory whose name is NOT a
			// valid UUID and whose metadata.json carries no ClydeUUID. This is
			// the exact shape produced by older Clyde versions before ClydeUUID
			// became the storage key.
			legacyKey := "legacy-slug-session"
			legacyDir := config.GetSessionDir(clydeRoot, legacyKey)
			Expect(util.EnsureDir(legacyDir)).To(Succeed())

			now := time.Now().UTC()
			metadata := session.Metadata{
				Name:         "Legacy Session",
				SessionID:    "uuid-legacy-1",
				Provider:     session.ProviderClaude,
				Created:      now,
				LastAccessed: now,
			}
			Expect(util.WriteJSON(filepath.Join(legacyDir, "metadata.json"), metadata)).To(Succeed())

			// Load the session via Get so we go through the on-disk normalization
			// path (which assigns storageKey=legacyKey but does NOT backfill
			// ClydeUUID, because legacyKey is not a UUID).
			loaded, err := store.Get("Legacy Session")
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.StorageKey()).To(Equal(legacyKey))
			Expect(loaded.Metadata.ClydeUUID).To(BeEmpty())

			// Touch the session and call Update. The Update path should mint a
			// fresh ClydeUUID, migrate the on-disk dir to the new UUID, and
			// remove the legacy dir.
			loaded.UpdateLastAccessed()
			Expect(store.Update(loaded)).To(Succeed())

			Expect(loaded.Metadata.ClydeUUID).NotTo(BeEmpty())
			Expect(loaded.StorageKey()).To(Equal(loaded.Metadata.ClydeUUID))

			newDir := config.GetSessionDir(clydeRoot, loaded.Metadata.ClydeUUID)
			Expect(util.DirExists(newDir)).To(BeTrue())
			Expect(util.DirExists(legacyDir)).To(BeFalse())

			// A subsequent Get must reach the migrated dir and report the same
			// ClydeUUID.
			retrieved, err := store.Get("Legacy Session")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Metadata.ClydeUUID).To(Equal(loaded.Metadata.ClydeUUID))
			Expect(retrieved.StorageKey()).To(Equal(loaded.Metadata.ClydeUUID))
		})
	})

	Describe("Resolve tier 1 (display title)", func() {
		It("resolves by exact display title when display title differs from name", func() {
			s := session.NewSession("auto-generated-name", "uuid-tier1-display")
			s.Metadata.Title = "Provider Owned Title"
			Expect(store.Create(s)).To(Succeed())

			resolved, err := store.Resolve("Provider Owned Title")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Name).To(Equal("auto-generated-name"))
			Expect(resolved.Metadata.Title).To(Equal("Provider Owned Title"))
		})

		It("returns nil when the same display title is shared by multiple sessions", func() {
			s1 := session.NewSession("session-one", "uuid-tier1-amb-1")
			s1.Metadata.Title = "Shared Title"
			Expect(store.Create(s1)).To(Succeed())

			s2 := session.NewSession("session-two", "uuid-tier1-amb-2")
			s2.Metadata.Title = "Shared Title"
			Expect(store.Create(s2)).To(Succeed())

			// "Shared Title" is not a valid Name match (Get fails), no session
			// has it as a direct id, and the display-title match is ambiguous.
			// Tier 3 substring matches both, so the call falls all the way
			// through to nil.
			resolved, err := store.Resolve("Shared Title")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).To(BeNil())
		})
	})

	Describe("Resolve tier 3 (substring)", func() {
		It("resolves a single substring match against the name", func() {
			Expect(store.Create(session.NewSession("Merry Swan", "uuid-tier3-1"))).To(Succeed())
			Expect(store.Create(session.NewSession("Quiet Falcon", "uuid-tier3-2"))).To(Succeed())

			resolved, err := store.Resolve("Merry")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Name).To(Equal("Merry Swan"))
		})

		It("resolves a single substring match against the context field", func() {
			s := session.NewSession("Generic Name", "uuid-tier3-ctx")
			s.Metadata.Context = "tracking-ticket-XYZ-789"
			Expect(store.Create(s)).To(Succeed())
			Expect(store.Create(session.NewSession("Other", "uuid-tier3-other"))).To(Succeed())

			resolved, err := store.Resolve("XYZ-789")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Name).To(Equal("Generic Name"))
		})

		It("returns nil when a substring matches more than one session", func() {
			Expect(store.Create(session.NewSession("Merry Swan", "uuid-tier3-amb-1"))).To(Succeed())
			Expect(store.Create(session.NewSession("Merry Falcon", "uuid-tier3-amb-2"))).To(Succeed())

			resolved, err := store.Resolve("Merry")
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).To(BeNil())
		})
	})

	Describe("SyncDiscoveryResults skip filters", func() {
		const providerUUID = "11111111-2222-3333-4444-555555555555"

		buildResult := func(modify func(*session.DiscoveryResult)) session.DiscoveryResult {
			r := session.DiscoveryResult{
				Provider: session.ProviderClaude,
				Identity: session.ProviderSessionID{
					Provider: session.ProviderClaude,
					ID:       providerUUID,
				},
				WorkspaceRoot:       "/Users/test/project",
				Entrypoint:          "cli",
				FirstEntryTime:      time.Date(2026, 4, 12, 23, 52, 12, 0, time.UTC),
				NameContract:        providerNameStub{value: "Should Not Adopt"},
				PrimaryArtifact:     "/tmp/transcript.jsonl",
				PrimaryArtifactKind: "transcript",
			}
			if modify != nil {
				modify(&r)
			}
			return r
		}

		It("skips IsAutoName results", func() {
			r := buildResult(func(r *session.DiscoveryResult) {
				r.IsAutoName = true
			})
			changes, err := store.SyncDiscoveryResults([]session.DiscoveryResult{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(changes).To(BeEmpty())

			sessions, listErr := store.List()
			Expect(listErr).NotTo(HaveOccurred())
			Expect(sessions).To(BeEmpty())
		})

		It("skips IsSubagent results", func() {
			r := buildResult(func(r *session.DiscoveryResult) {
				r.IsSubagent = true
			})
			changes, err := store.SyncDiscoveryResults([]session.DiscoveryResult{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(changes).To(BeEmpty())

			sessions, listErr := store.List()
			Expect(listErr).NotTo(HaveOccurred())
			Expect(sessions).To(BeEmpty())
		})

		It("skips results with an empty provider session id", func() {
			r := buildResult(func(r *session.DiscoveryResult) {
				r.Identity = session.ProviderSessionID{Provider: session.ProviderClaude, ID: ""}
			})
			changes, err := store.SyncDiscoveryResults([]session.DiscoveryResult{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(changes).To(BeEmpty())

			sessions, listErr := store.List()
			Expect(listErr).NotTo(HaveOccurred())
			Expect(sessions).To(BeEmpty())
		})

		It("skips results whose workspace root is a clyde scratch directory", func() {
			r := buildResult(func(r *session.DiscoveryResult) {
				r.WorkspaceRoot = "/Users/test/Library/Caches/clyde/context-scratch"
			})
			changes, err := store.SyncDiscoveryResults([]session.DiscoveryResult{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(changes).To(BeEmpty())

			sessions, listErr := store.List()
			Expect(listErr).NotTo(HaveOccurred())
			Expect(sessions).To(BeEmpty())
		})

		It("does adopt a clean discovery result, confirming the skip filters above are filter-specific", func() {
			r := buildResult(nil)
			changes, err := store.SyncDiscoveryResults([]session.DiscoveryResult{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(changes).To(HaveLen(1))
			Expect(changes[0].Adopted).To(BeTrue())
			Expect(changes[0].Session.Metadata.ProviderSessionID()).To(Equal(providerUUID))
		})
	})
})
