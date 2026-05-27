package session_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/claude/claudepath"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

var _ = Describe("MigrateTranscriptPath", func() {
	var (
		tempHome    string
		clydeRoot   string
		sessionsDir string
	)

	BeforeEach(func() {
		tempHome = GinkgoT().TempDir()
		clydeRoot = filepath.Join(tempHome, ".local", "share", "clyde")
		sessionsDir = filepath.Join(clydeRoot, config.SessionsDir)
		Expect(util.EnsureDir(sessionsDir)).To(Succeed())
	})

	It("rewrites a rotator-prefixed transcriptPath to the canonical path", func() {
		sessionID := "0630f096-a323-42e3-bbb1-03730cfe24b0"
		workspaceRoot := "/Users/test/Sites/configs"
		clydeUUID := "254e85eb-955e-4caa-93dc-a2b4b25cb177"
		sessionDir := filepath.Join(sessionsDir, clydeUUID)
		Expect(util.EnsureDir(sessionDir)).To(Succeed())
		stalePath := "/var/folders/jq/T/clyde-501/rotator-launch/anthropic-1747377084/projects/-Users-test-Sites-configs/" + sessionID + ".jsonl"
		metadataPath := filepath.Join(sessionDir, "metadata.json")
		body := map[string]any{
			"name":           "read-only-diagnose-why-ipv6-not",
			"clydeUuid":      clydeUUID,
			"provider":       "claude",
			"sessionId":      sessionID,
			"transcriptPath": stalePath,
			"providerState": map[string]any{
				"current":   map[string]any{"provider": "claude", "id": sessionID},
				"artifacts": map[string]any{"transcriptPath": stalePath},
			},
			"workDir":       workspaceRoot,
			"workspaceRoot": workspaceRoot,
		}
		raw, err := json.MarshalIndent(body, "", "  ")
		Expect(err).ToNot(HaveOccurred())
		Expect(os.WriteFile(metadataPath, raw, 0o644)).To(Succeed())

		result, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Scanned).To(Equal(1))
		Expect(result.Migrated).To(Equal(1))
		Expect(result.Failed).To(Equal(0))

		updated, err := os.ReadFile(metadataPath)
		Expect(err).ToNot(HaveOccurred())
		var decoded map[string]any
		Expect(json.Unmarshal(updated, &decoded)).To(Succeed())
		canonical := claudepath.TranscriptPathForWorkspace(tempHome, workspaceRoot, sessionID)
		Expect(decoded["transcriptPath"]).To(Equal(canonical))
		providerState := decoded["providerState"].(map[string]any)
		artifacts := providerState["artifacts"].(map[string]any)
		Expect(artifacts["transcriptPath"]).To(Equal(canonical))

		// Untouched fields survive the rewrite.
		Expect(decoded["name"]).To(Equal("read-only-diagnose-why-ipv6-not"))
		Expect(decoded["workDir"]).To(Equal(workspaceRoot))
		Expect(decoded["workspaceRoot"]).To(Equal(workspaceRoot))
	})

	It("is idempotent across repeated runs", func() {
		sessionID := "uuid-idempotent"
		workspaceRoot := "/Users/test/Sites/proj"
		sessionDir := filepath.Join(sessionsDir, "clyde-uuid-idempotent")
		Expect(util.EnsureDir(sessionDir)).To(Succeed())
		stalePath := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"
		writeMetadata(filepath.Join(sessionDir, "metadata.json"), sessionID, workspaceRoot, stalePath)

		first, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(first.Migrated).To(Equal(1))

		second, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(second.Migrated).To(Equal(0))
		Expect(second.Skipped).To(Equal(1))
	})

	It("leaves healthy non-rotator paths untouched", func() {
		sessionID := "uuid-healthy"
		workspaceRoot := "/Users/test/Sites/proj"
		sessionDir := filepath.Join(sessionsDir, "clyde-uuid-healthy")
		Expect(util.EnsureDir(sessionDir)).To(Succeed())
		canonical := claudepath.TranscriptPathForWorkspace(tempHome, workspaceRoot, sessionID)
		Expect(util.EnsureDir(filepath.Dir(canonical))).To(Succeed())
		Expect(os.WriteFile(canonical, []byte("x"), 0o644)).To(Succeed())
		writeMetadata(filepath.Join(sessionDir, "metadata.json"), sessionID, workspaceRoot, canonical)

		result, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Migrated).To(Equal(0))
		Expect(result.Skipped).To(Equal(1))
	})

	It("skips records with no workspaceRoot", func() {
		sessionDir := filepath.Join(sessionsDir, "clyde-uuid-no-workspace")
		Expect(util.EnsureDir(sessionDir)).To(Succeed())
		stalePath := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-x/uuid.jsonl"
		writeMetadata(filepath.Join(sessionDir, "metadata.json"), "uuid-no-ws", "", stalePath)

		result, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Migrated).To(Equal(0))
		Expect(result.Skipped).To(Equal(1))
	})

	It("skips non-claude providers", func() {
		sessionDir := filepath.Join(sessionsDir, "clyde-uuid-codex")
		Expect(util.EnsureDir(sessionDir)).To(Succeed())
		body := map[string]any{
			"name":           "codex-session",
			"provider":       "codex",
			"sessionId":      "uuid-codex",
			"workspaceRoot":  "/Users/test/Sites/proj",
			"transcriptPath": "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-x/uuid.jsonl",
		}
		raw, err := json.MarshalIndent(body, "", "  ")
		Expect(err).ToNot(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(sessionDir, "metadata.json"), raw, 0o644)).To(Succeed())

		result, err := session.MigrateTranscriptPath(clydeRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Migrated).To(Equal(0))
		Expect(result.Skipped).To(Equal(1))
	})

	It("returns a zero result when the sessions directory does not exist", func() {
		emptyRoot := filepath.Join(tempHome, "no-such")
		result, err := session.MigrateTranscriptPath(emptyRoot, tempHome)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Scanned).To(Equal(0))
	})
})

func writeMetadata(path, sessionID, workspaceRoot, transcriptPath string) {
	body := map[string]any{
		"name":           "test-session",
		"provider":       "claude",
		"sessionId":      sessionID,
		"workspaceRoot":  workspaceRoot,
		"workDir":        workspaceRoot,
		"transcriptPath": transcriptPath,
		"providerState": map[string]any{
			"current":   map[string]any{"provider": "claude", "id": sessionID},
			"artifacts": map[string]any{"transcriptPath": transcriptPath},
		},
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(path, raw, 0o644)).To(Succeed())
}
