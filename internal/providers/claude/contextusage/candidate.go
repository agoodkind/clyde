package contextusage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/contextusage"
)

// claudeCandidateProber adapts the Claude /context probe machinery to
// the generic CandidateProber contract. The probe writes the candidate
// JSONL to a disposable file under the live session's project directory
// (~/.claude/projects/<encoded-path>/), spawns claude --resume against
// the disposable session id, parses the Messages category from the
// returned snapshot, and removes the disposable file before returning.
//
// Writing the candidate next to the live session is the cleanest of the
// approaches considered. Claude resolves project context from the file
// system, so the candidate's /context view reflects the same workspace,
// memory files, skills, and system prompt the user will see on resume.
// The disposable file is unlinked on every return path, so traces under
// ~/.claude/projects/ are bounded to a single probe's lifetime.
type claudeCandidateProber struct{}

// messagesCategory is the /context row name whose tokens the planner
// projects against. Other categories are static overhead from the
// planner's perspective and stay constant across iterations.
const messagesCategory = "Messages"

// CountCandidate writes the candidate to a disposable jsonl, spawns the
// Claude probe against it, and returns the Messages token count. The
// temp file is always unlinked on return.
func (claudeCandidateProber) CountCandidate(ctx context.Context, req contextusage.CandidateRequest) (int, error) {
	log := sessionContextLog.Logger()
	if strings.TrimSpace(req.TranscriptPath) == "" {
		log.ErrorContext(ctx, "compact.candidate.invalid_request",
			"component", "compact",
			"subcomponent", "candidate",
			"reason", "empty_transcript_path",
			"err", "empty_transcript_path",
		)
		return 0, errors.New("candidate probe: empty transcript path")
	}
	if len(req.CandidateJSONL) == 0 {
		log.ErrorContext(ctx, "compact.candidate.invalid_request",
			"component", "compact",
			"subcomponent", "candidate",
			"reason", "empty_candidate_jsonl",
			"err", "empty_candidate_jsonl",
		)
		return 0, errors.New("candidate probe: empty candidate jsonl")
	}
	projectDir := filepath.Dir(req.TranscriptPath)
	if projectDir == "" || projectDir == "." {
		log.ErrorContext(ctx, "compact.candidate.invalid_request",
			"component", "compact",
			"subcomponent", "candidate",
			"reason", "project_dir_unresolvable",
			"transcript_path", req.TranscriptPath,
			"err", "project_dir_unresolvable",
		)
		return 0, fmt.Errorf("candidate probe: cannot derive project dir from transcript path %q", req.TranscriptPath)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		log.ErrorContext(ctx, "compact.candidate.mkdir_failed",
			"component", "compact",
			"subcomponent", "candidate",
			"project_dir", projectDir,
			"err", err,
		)
		return 0, fmt.Errorf("candidate probe: ensure project dir: %w", err)
	}
	candidateID := uuid.NewString()
	candidatePath := filepath.Join(projectDir, candidateID+".jsonl")
	if err := writeCandidateFile(ctx, candidatePath, req.CandidateJSONL); err != nil {
		return 0, err
	}
	defer removeCandidateFile(ctx, candidatePath)

	log.InfoContext(ctx, "compact.candidate.probe_started",
		"component", "compact",
		"subcomponent", "candidate",
		"candidate_id", candidateID,
		"project_dir", projectDir,
		"work_dir", req.WorkDir,
		"candidate_bytes", len(req.CandidateJSONL),
	)
	snapshot, err := ProbeContextUsage(ctx, ProbeOptions{
		SessionID:   candidateID,
		WorkDir:     req.WorkDir,
		Binary:      "",
		Timeout:     0,
		ForkSession: true,
	})
	if err != nil {
		log.WarnContext(ctx, "compact.candidate.probe_failed",
			"component", "compact",
			"subcomponent", "candidate",
			"candidate_id", candidateID,
			"err", err,
		)
		return 0, fmt.Errorf("candidate probe: %w", err)
	}
	tokens := 0
	for _, cat := range snapshot.Categories {
		if cat.Name == messagesCategory {
			tokens += cat.Tokens
		}
	}
	if tokens <= 0 {
		log.WarnContext(ctx, "compact.candidate.zero_messages",
			"component", "compact",
			"subcomponent", "candidate",
			"candidate_id", candidateID,
			"category", messagesCategory,
		)
		return 0, fmt.Errorf("candidate probe: zero %s tokens in snapshot", messagesCategory)
	}
	log.InfoContext(ctx, "compact.candidate.probe_completed",
		"component", "compact",
		"subcomponent", "candidate",
		"candidate_id", candidateID,
		"messages_tokens", tokens,
	)
	return tokens, nil
}

// writeCandidateFile creates the disposable jsonl with restrictive perms
// matching Claude's own session files. The trailing newline is appended
// when missing so the JSONL parser inside claude sees a well-formed
// terminator on the final line.
func writeCandidateFile(ctx context.Context, path string, body []byte) error {
	log := sessionContextLog.Logger()
	payload := body
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		payload = append(payload, '\n')
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		log.ErrorContext(ctx, "compact.candidate.write_failed",
			"component", "compact",
			"subcomponent", "candidate",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("candidate probe: write candidate file: %w", err)
	}
	return nil
}

// removeCandidateFile unlinks the disposable jsonl. Failures are logged
// at debug because a leftover candidate is a minor cleanup miss, not a
// correctness bug; the next planner run still writes its own UUID file.
func removeCandidateFile(ctx context.Context, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		sessionContextLog.Logger().DebugContext(ctx, "compact.candidate.cleanup_failed",
			"component", "compact",
			"subcomponent", "candidate",
			"path", path,
			"err", err,
		)
	}
}

func init() {
	contextusage.RegisterCandidate(claudeProberID, claudeCandidateProber{})
}
