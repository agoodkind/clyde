package session_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/session"
)

var _ = Describe("ScanProjects", func() {
	var (
		projectsRoot string
		projDir      string
	)

	BeforeEach(func() {
		registry.RegisterDefaultDiscoveryScanners()
		projectsRoot = GinkgoT().TempDir()
		// One project dir under ~/.claude/projects.
		projDir = filepath.Join(projectsRoot, "-Users-agoodkind-Sites-foo")
		Expect(os.MkdirAll(projDir, 0o755)).To(Succeed())
	})

	writeTranscript := func(uuid, body string) string {
		path := filepath.Join(projDir, uuid+".jsonl")
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
		return path
	}

	It("returns one DiscoveryResult per transcript with first-entry metadata", func() {
		writeTranscript("aaaaaaaa-1111-2222-3333-444444444444",
			`{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"/Users/agoodkind/Sites/foo","sessionId":"aaaaaaaa-1111-2222-3333-444444444444"}`+"\n",
		)

		out, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))

		r := out[0]
		Expect(r.ProviderSessionID()).To(Equal("aaaaaaaa-1111-2222-3333-444444444444"))
		Expect(r.WorkspaceRoot).To(Equal("/Users/agoodkind/Sites/foo"))
		Expect(r.Entrypoint).To(Equal("cli"))
		Expect(r.IsAutoName).To(BeFalse())
		Expect(r.IsForked).To(BeFalse())
		Expect(r.IsSubagent).To(BeFalse())
		Expect(r.FirstEntryTime.IsZero()).To(BeFalse())
	})

	It("captures fork lineage from transcript headers", func() {
		writeTranscript("bbbbbbbb-1111-2222-3333-444444444444",
			`{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"/Users/agoodkind/Sites/foo","sessionId":"bbbbbbbb-1111-2222-3333-444444444444","forkedFrom":{"sessionId":"aaaaaaaa-1111-2222-3333-444444444444"}}`+"\n",
		)

		out, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].IsForked).To(BeTrue())
		Expect(out[0].ForkParent.ID).To(Equal("aaaaaaaa-1111-2222-3333-444444444444"))
	})

	It("flags sdk-cli entrypoints as auto-name", func() {
		writeTranscript("bbbbbbbb-1111-2222-3333-444444444444",
			`{"type":"queue-operation","content":"You output exactly ONE token. That token is a kebab-case name. Output ONLY the kebab-case name."}`+"\n"+
				`{"type":"user","timestamp":"2026-04-15T10:00:00Z","entrypoint":"sdk-cli","cwd":"/x","sessionId":"bbbbbbbb-1111-2222-3333-444444444444"}`+"\n",
		)
		out, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].IsAutoName).To(BeTrue())
	})

	It("flags transcripts inside subagents/ subdirectories", func() {
		subDir := filepath.Join(projDir, "subagents")
		Expect(os.MkdirAll(subDir, 0o755)).To(Succeed())
		path := filepath.Join(subDir, "agent-aabbcc.jsonl")
		Expect(os.WriteFile(path, []byte(
			`{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"/x","sessionId":"cccccccc-1111-2222-3333-444444444444"}`+"\n",
		), 0o600)).To(Succeed())

		out, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].IsSubagent).To(BeTrue())
	})

	It("skips files with no recognizable session entry", func() {
		writeTranscript("ddd", `not json`+"\n")
		out, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(BeEmpty())
	})
})

var _ = Describe("AdoptUnknown", func() {
	var (
		projectsRoot string
		store        *session.FileStore
	)

	BeforeEach(func() {
		registry.RegisterDefaultDiscoveryScanners()
		projectsRoot = GinkgoT().TempDir()
		clydeRoot := GinkgoT().TempDir()
		store = session.NewFileStore(clydeRoot)
	})

	writeTranscript := func(uuid, cwd string) string {
		dir := filepath.Join(projectsRoot, "-Users-agoodkind-Sites-foo")
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		path := filepath.Join(dir, uuid+".jsonl")
		body := `{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"` + cwd + `","sessionId":"` + uuid + `"}` + "\n"
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
		return path
	}

	It("creates a registry entry for an unknown transcript", func() {
		writeTranscript("aaaaaaaa-1111-2222-3333-444444444444", "/Users/agoodkind/Sites/foo")
		results, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())

		adopted, err := session.AdoptUnknown(store, results)
		Expect(err).ToNot(HaveOccurred())
		Expect(adopted).To(HaveLen(1))
		Expect(adopted[0].Name).To(HavePrefix("foo-"))
		Expect(adopted[0].Metadata.ProviderSessionID()).To(Equal("aaaaaaaa-1111-2222-3333-444444444444"))
	})

	It("does not re-adopt a known UUID", func() {
		uuid := "aaaaaaaa-1111-2222-3333-444444444444"
		writeTranscript(uuid, "/Users/agoodkind/Sites/foo")
		Expect(store.Create(&session.Session{
			Name:     "existing",
			Metadata: session.Metadata{Name: "existing", SessionID: uuid},
		})).To(Succeed())

		results, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		adopted, err := session.AdoptUnknown(store, results)
		Expect(err).ToNot(HaveOccurred())
		Expect(adopted).To(BeEmpty())
	})

	It("marks adopted fork sessions and links the parent when known", func() {
		parentUUID := "aaaaaaaa-1111-2222-3333-444444444444"
		childUUID := "bbbbbbbb-1111-2222-3333-444444444444"
		Expect(store.Create(&session.Session{
			Name: "Parent Exact Name",
			Metadata: session.Metadata{
				Name:         "Parent Exact Name",
				SessionID:    parentUUID,
				DisplayTitle: "Stale Provider Title",
			},
		})).To(Succeed())
		dir := filepath.Join(projectsRoot, "-Users-agoodkind-Sites-foo")
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		path := filepath.Join(dir, childUUID+".jsonl")
		body := `{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"/Users/agoodkind/Sites/foo","sessionId":"` + childUUID + `","forkedFrom":{"sessionId":"` + parentUUID + `"}}` + "\n"
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())

		results, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		adopted, err := session.AdoptUnknown(store, results)
		Expect(err).ToNot(HaveOccurred())
		Expect(adopted).To(HaveLen(1))
		Expect(adopted[0].Metadata.IsForkedSession).To(BeTrue())
		Expect(adopted[0].Metadata.ParentSession).To(Equal("Parent Exact Name"))
		Expect(adopted[0].Metadata.ParentClydeUUID).ToNot(BeEmpty())
	})

	It("skips auto-name and subagent transcripts", func() {
		dir := filepath.Join(projectsRoot, "-Users-agoodkind-Sites-foo")
		Expect(os.MkdirAll(filepath.Join(dir, "subagents"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "subagents", "agent-x.jsonl"),
			[]byte(`{"type":"system","timestamp":"2026-04-15T10:00:00Z","entrypoint":"cli","cwd":"/x","sessionId":"sub11111-1111-2222-3333-444444444444"}`+"\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "auto.jsonl"),
			[]byte(`{"type":"queue-operation","content":"kebab-case name. Output ONLY"}`+"\n"+
				`{"type":"user","timestamp":"2026-04-15T10:00:00Z","entrypoint":"sdk-cli","cwd":"/x","sessionId":"auto1111-1111-2222-3333-444444444444"}`+"\n"), 0o600)).To(Succeed())

		results, err := session.ScanProjects(projectsRoot)
		Expect(err).ToNot(HaveOccurred())
		adopted, err := session.AdoptUnknown(store, results)
		Expect(err).ToNot(HaveOccurred())
		Expect(adopted).To(BeEmpty())
	})
})
