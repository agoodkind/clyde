package session

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type tier4TestScanner struct {
	results *[]DiscoveryResult
}

type tier4TestName struct {
	value string
}

func (scanner tier4TestScanner) Provider() ProviderID {
	return ProviderClaude
}

func (scanner tier4TestScanner) Scan() ([]DiscoveryResult, error) {
	return append([]DiscoveryResult(nil), (*scanner.results)...), nil
}

func (name tier4TestName) GetName() string {
	return name.value
}

func (name tier4TestName) Rename(_ string, taken map[string]bool) string {
	candidate := UniqueDisplayName(name.value, taken)
	if candidate == "" || ValidateDisplayName(candidate) != nil {
		return ""
	}
	return candidate
}

var _ = Describe("Resolve tier 4 (transparent adoption)", func() {
	var (
		clydeRoot   string
		projDir     string
		store       *FileStore
		discoveries []DiscoveryResult
	)

	const uuid = "22a95bc5-eb24-4302-8f2f-2253bc587cb5"

	BeforeEach(func() {
		clydeRoot = GinkgoT().TempDir()
		projectsRoot := GinkgoT().TempDir()
		projDir = filepath.Join(projectsRoot, "-Users-agoodkind-Sites-tack")
		Expect(os.MkdirAll(projDir, 0o755)).To(Succeed())

		Expect(os.MkdirAll(filepath.Join(clydeRoot, "sessions"), 0o755)).To(Succeed())
		discoveries = nil

		store = &FileStore{
			clydeRoot:      clydeRoot,
			discoveryCache: newDiscoveryCache([]DiscoveryScanner{tier4TestScanner{results: &discoveries}}, 0),
		}
	})

	writeTranscript := func(id, customTitle string) {
		body := ""
		if customTitle != "" {
			body += `{"type":"custom-title","customTitle":"` + customTitle + `","sessionId":"` + id + `"}` + "\n"
		}
		body += `{"type":"system","timestamp":"2026-04-12T23:52:12Z","entrypoint":"cli","cwd":"/Users/agoodkind/Sites/tack","sessionId":"` + id + `"}` + "\n"
		path := filepath.Join(projDir, id+".jsonl")
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
		discoveries = append(discoveries, DiscoveryResult{
			Provider: ProviderClaude,
			Identity: ProviderSessionID{
				Provider: ProviderClaude,
				ID:       id,
			},
			WorkspaceRoot:       "/Users/agoodkind/Sites/tack",
			Entrypoint:          "cli",
			FirstEntryTime:      time.Date(2026, time.April, 12, 23, 52, 12, 0, time.UTC),
			NameContract:        tier4TestName{value: customTitle},
			PrimaryArtifact:     path,
			PrimaryArtifactKind: "transcript",
		})
	}

	It("adopts by exact customTitle on tier-1 miss", func() {
		writeTranscript(uuid, "2026-04-12 Merry Swan")

		sess, err := store.Resolve("2026-04-12 Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).ToNot(BeNil())
		Expect(sess.Name).To(Equal("2026-04-12 Merry Swan"))
		Expect(sess.Metadata.ProviderSessionID()).To(Equal(uuid))
		Expect(sess.Metadata.Title).To(Equal("2026-04-12 Merry Swan"))

		metaPath := filepath.Join(clydeRoot, "sessions", sess.StorageKey(), "metadata.json")
		_, statErr := os.Stat(metaPath)
		Expect(statErr).ToNot(HaveOccurred(), "metadata.json should be written at %s", metaPath)
	})

	It("adopts by bare UUID and uses the exact title as Name", func() {
		writeTranscript(uuid, "2026-04-12 Merry Swan")

		sess, err := store.Resolve(uuid)
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).ToNot(BeNil())
		Expect(sess.Name).To(Equal("2026-04-12 Merry Swan"))
		Expect(sess.Metadata.ProviderSessionID()).To(Equal(uuid))
	})

	It("falls back to workspace-plus-UUID when customTitle is absent", func() {
		writeTranscript(uuid, "")

		sess, err := store.Resolve(uuid)
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).ToNot(BeNil())
		Expect(sess.Name).To(ContainSubstring("tack"))
		Expect(sess.Name).To(ContainSubstring(uuid[:8]))
		Expect(sess.Metadata.Title).To(Equal(""))
	})

	It("falls back to workspace-plus-UUID when customTitle violates display-name policy", func() {
		writeTranscript(uuid, "bad\nname")

		sess, err := store.Resolve(uuid)
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).ToNot(BeNil())
		Expect(sess.Name).To(ContainSubstring("tack"))
		Expect(sess.Name).To(ContainSubstring(uuid[:8]))
	})

	It("returns nil on no match without error", func() {
		sess, err := store.Resolve("never-existed")
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).To(BeNil())
	})

	It("is idempotent: second resolve for the same name hits tier 1", func() {
		writeTranscript(uuid, "Merry Swan")

		first, err := store.Resolve("Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(first).ToNot(BeNil())

		second, err := store.Resolve("Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(second).ToNot(BeNil())
		Expect(second.Name).To(Equal(first.Name))
		Expect(second.Metadata.ProviderSessionID()).To(Equal(first.Metadata.ProviderSessionID()))
	})

	It("reconciles an already-known session idempotently when the provider name appears later", func() {
		writeTranscript(uuid, "Merry Swan")
		Expect(store.Create(&Session{
			Name: "tack-22a95bc5",
			Metadata: Metadata{
				Name:      "tack-22a95bc5",
				Provider:  ProviderClaude,
				SessionID: uuid,
			},
		})).To(Succeed())

		first, err := store.Resolve("Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(first).ToNot(BeNil())
		Expect(first.Name).To(Equal("Merry Swan"))
		Expect(first.Metadata.Title).To(Equal(""))

		second, err := store.Resolve("Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(second).ToNot(BeNil())
		Expect(second.Name).To(Equal("Merry Swan"))
		Expect(second.Metadata.Title).To(Equal(""))
	})

	It("syncs a provider rename for an already-known session", func() {
		existing := NewSession("tack-22a95bc5", uuid)
		Expect(store.Create(existing)).To(Succeed())

		results := []DiscoveryResult{{
			Provider: ProviderClaude,
			Identity: ProviderSessionID{
				Provider: ProviderClaude,
				ID:       uuid,
			},
			WorkspaceRoot:       "/Users/agoodkind/Sites/tack",
			Entrypoint:          "cli",
			FirstEntryTime:      time.Date(2026, time.April, 12, 23, 52, 12, 0, time.UTC),
			NameContract:        tier4TestName{value: "Renamed in Provider"},
			PrimaryArtifact:     filepath.Join(projDir, uuid+".jsonl"),
			PrimaryArtifactKind: "transcript",
		}}

		changes, err := store.SyncDiscoveryResults(results)
		Expect(err).ToNot(HaveOccurred())
		Expect(changes).To(HaveLen(1))
		Expect(changes[0].OldName).To(Equal("tack-22a95bc5"))
		Expect(changes[0].Session.Name).To(Equal("Renamed in Provider"))
		Expect(changes[0].Session.Metadata.Title).To(Equal(""))

		reloaded, err := store.Get("Renamed in Provider")
		Expect(err).ToNot(HaveOccurred())
		Expect(reloaded.Metadata.Title).To(Equal(""))
		Expect(reloaded.ClydeUUID()).To(Equal(existing.ClydeUUID()))
	})

	It("skips tier 4 when the store is constructed read-only", func() {
		writeTranscript(uuid, "Merry Swan")
		readOnly := &FileStore{clydeRoot: clydeRoot, noAdopt: true}

		sess, err := readOnly.Resolve("Merry Swan")
		Expect(err).ToNot(HaveOccurred())
		Expect(sess).To(BeNil(), "read-only store must not adopt even when a matching transcript exists")
	})
})
