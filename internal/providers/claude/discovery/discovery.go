// Package discovery scans Claude transcript artifacts for adoptable sessions.
package discovery

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/session"
)

const primaryArtifactKindTranscript = "transcript"

type transcriptHeader struct {
	SessionID   string `json:"sessionId"`
	CWD         string `json:"cwd"`
	Entrypoint  string `json:"entrypoint"`
	Timestamp   string `json:"timestamp"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	CustomTitle string `json:"customTitle"`
	ForkedFrom  struct {
		SessionID string `json:"sessionId"`
	} `json:"forkedFrom"`
}

type Scanner struct {
	projectsDir string
}

func NewScanner(projectsDir string) Scanner {
	return Scanner{projectsDir: projectsDir}
}

func (s Scanner) Provider() session.ProviderID {
	return session.ProviderClaude
}

func (s Scanner) Scan() ([]session.DiscoveryResult, error) {
	var out []session.DiscoveryResult
	err := filepath.WalkDir(s.projectsDir, func(path string, directoryEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsPermission(walkErr) {
				return nil
			}
			return walkErr
		}
		if directoryEntry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(directoryEntry.Name(), ".jsonl") {
			return nil
		}
		discoveryResult, ok := ReadTranscriptHeader(path)
		if !ok {
			return nil
		}
		out = append(out, discoveryResult)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s Scanner) DiscoveryScannerForHome(homeDir string) session.DiscoveryScanner {
	return NewScanner(ProjectsRoot(homeDir))
}

func (s Scanner) DiscoveryScannerForRoot(root string) session.DiscoveryScanner {
	return NewScanner(root)
}

func ProjectsRoot(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "projects")
}

// ReadTranscriptHeader reads a Claude transcript JSONL file and folds its
// header records into a DiscoveryResult. Returns false when the file lacks
// an identifying session id.
func ReadTranscriptHeader(path string) (session.DiscoveryResult, bool) {
	file, err := os.Open(path)
	if err != nil {
		return session.DiscoveryResult{}, false
	}
	defer func() { _ = file.Close() }()

	discoveryResult := newDiscoveryResultForPath(path)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var header transcriptHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}
		if applyTranscriptHeader(&discoveryResult, header) {
			break
		}
	}
	if discoveryResult.ProviderIdentity().IsZero() {
		return session.DiscoveryResult{}, false
	}
	if discoveryResult.Entrypoint == "sdk-cli" {
		discoveryResult.IsAutoName = true
	}
	return discoveryResult, true
}

// newDiscoveryResultForPath seeds the discovery result with the artifact
// path and the subagent flag derived from the directory layout.
func newDiscoveryResultForPath(path string) session.DiscoveryResult {
	discoveryResult := session.DiscoveryResult{
		Provider:            session.ProviderClaude,
		PrimaryArtifact:     path,
		PrimaryArtifactKind: primaryArtifactKindTranscript,
	}
	if strings.Contains(path, string(os.PathSeparator)+"subagents"+string(os.PathSeparator)) {
		discoveryResult.IsSubagent = true
	}
	return discoveryResult
}

// applyTranscriptHeader folds one parsed JSONL record into the discovery
// result and returns true once every header field of interest is filled.
func applyTranscriptHeader(out *session.DiscoveryResult, header transcriptHeader) bool {
	switch header.Type {
	case "queue-operation":
		if !out.IsAutoName && looksLikeAutoNamePrompt(header.Content) {
			out.IsAutoName = true
		}
		return false
	case "custom-title":
		applyCustomTitleHeader(out, header)
		return false
	}
	applyIdentityHeader(out, header)
	applyMetadataHeader(out, header)
	return !out.ProviderIdentity().IsZero() &&
		out.WorkspaceRoot != "" &&
		out.Entrypoint != "" &&
		!out.FirstEntryTime.IsZero()
}

// applyCustomTitleHeader copies the custom title plus any newly observed
// identity or fork pointer from a custom-title record.
func applyCustomTitleHeader(out *session.DiscoveryResult, header transcriptHeader) {
	if header.CustomTitle != "" {
		out.CustomTitle = header.CustomTitle
	}
	applyIdentityHeader(out, header)
}

// applyIdentityHeader records the session identity and fork-parent pointer
// the first time they appear in the transcript.
func applyIdentityHeader(out *session.DiscoveryResult, header transcriptHeader) {
	if header.SessionID != "" && out.ProviderIdentity().IsZero() {
		out.Identity = session.ProviderSessionID{Provider: session.ProviderClaude, ID: header.SessionID}
	}
	if header.ForkedFrom.SessionID != "" && out.ForkParent.IsZero() {
		out.ForkParent = session.ProviderSessionID{Provider: session.ProviderClaude, ID: header.ForkedFrom.SessionID}
		out.IsForked = true
	}
}

// applyMetadataHeader fills cwd, entrypoint, and first-entry timestamp the
// first time each value appears.
func applyMetadataHeader(out *session.DiscoveryResult, header transcriptHeader) {
	if header.CWD != "" && out.WorkspaceRoot == "" {
		out.WorkspaceRoot = header.CWD
	}
	if header.Entrypoint != "" && out.Entrypoint == "" {
		out.Entrypoint = header.Entrypoint
	}
	if header.Timestamp != "" && out.FirstEntryTime.IsZero() {
		if parsedTime, parseErr := time.Parse(time.RFC3339, header.Timestamp); parseErr == nil {
			out.FirstEntryTime = parsedTime
		}
	}
}

func looksLikeAutoNamePrompt(content string) bool {
	if content == "" {
		return false
	}
	lowercaseContent := strings.ToLower(content)
	return strings.Contains(lowercaseContent, "kebab-case") && strings.Contains(lowercaseContent, "output only")
}
