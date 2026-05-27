package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/claude/claudepath"
	"goodkind.io/clyde/internal/util"
)

const (
	migrationTranscriptPathEventName = "session.metadata.transcript_path_migration_completed"
	rotatorLaunchPathSegment         = "/rotator-launch/"
	providerStateKey                 = "providerState"
	artifactsKey                     = "artifacts"
	transcriptPathKey                = "transcriptPath"
	sessionIDKey                     = "sessionId"
	workspaceRootKey                 = "workspaceRoot"
	providerKey                      = "provider"
)

// TranscriptPathMigrationResult reports the outcome of the rotator-leak rewrite.
type TranscriptPathMigrationResult struct {
	Scanned  int
	Migrated int
	Skipped  int
	Failed   int
}

// MigrateTranscriptPath rewrites session metadata files whose transcriptPath
// points into a cleaned-up OAuth-rotator launch directory. The canonical path
// is derived from workspaceRoot plus the provider session UUID, matching where
// claude actually persists the JSONL under <home>/.claude/projects/. Files with
// a healthy transcriptPath are left untouched, so the migration is idempotent.
func MigrateTranscriptPath(clydeRoot, homeDir string) (TranscriptPathMigrationResult, error) {
	result := TranscriptPathMigrationResult{
		Scanned:  0,
		Migrated: 0,
		Skipped:  0,
		Failed:   0,
	}
	homeDir = strings.TrimSpace(homeDir)
	if homeDir == "" {
		logTranscriptPathMigrationResult(result)
		return result, nil
	}
	sessionsDir := config.GetSessionsDir(clydeRoot)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			logTranscriptPathMigrationResult(result)
			return result, nil
		}
		sessionLog.Warn("session.metadata.transcript_path_migration_readdir_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", sessionsDir,
			"err", err,
		)
		return result, fmt.Errorf("read sessions directory for transcript path migration: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		result.Scanned++
		metadataPath := filepath.Join(sessionsDir, entry.Name(), metadataFile)
		migrated, migrateErr := migrateTranscriptPathFile(metadataPath, homeDir)
		if migrateErr != nil {
			result.Failed++
			sessionLog.Warn("session.metadata.transcript_path_migration_file_failed",
				"component", migrationComponent,
				"subcomponent", migrationSubcomponent,
				"path", metadataPath,
				"err", migrateErr,
			)
			continue
		}
		if migrated {
			result.Migrated++
			continue
		}
		result.Skipped++
	}
	logTranscriptPathMigrationResult(result)
	return result, nil
}

func migrateTranscriptPathFile(metadataPath, homeDir string) (bool, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		sessionLog.Warn("session.metadata.transcript_path_migration_read_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("read metadata file: %w", err)
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &fields); err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_decode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("decode metadata file: %w", err)
	}

	provider := decodeStringField(fields, providerKey)
	if provider != string(ProviderClaude) {
		return false, nil
	}
	sessionID := decodeStringField(fields, sessionIDKey)
	workspaceRoot := decodeStringField(fields, workspaceRootKey)
	if sessionID == "" || workspaceRoot == "" {
		return false, nil
	}

	canonical := claudepath.TranscriptPathForWorkspace(homeDir, workspaceRoot, sessionID)
	mutated := false

	currentTop := decodeStringField(fields, transcriptPathKey)
	if shouldRewriteTranscriptPath(currentTop, canonical) {
		encoded, err := json.Marshal(canonical)
		if err != nil {
			sessionLog.Warn("session.metadata.transcript_path_migration_encode_failed",
				"component", migrationComponent,
				"subcomponent", migrationSubcomponent,
				"path", metadataPath,
				"field", transcriptPathKey,
				"err", err,
			)
			return false, fmt.Errorf("encode canonical transcript path: %w", err)
		}
		fields[transcriptPathKey] = encoded
		mutated = true
	}

	providerStateMutated, err := rewriteProviderArtifactsTranscriptPath(metadataPath, fields, canonical)
	if err != nil {
		return false, err
	}
	if providerStateMutated {
		mutated = true
	}

	if !mutated {
		return false, nil
	}

	if err := util.WriteJSON(metadataPath, fields); err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_write_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("write migrated metadata file: %w", err)
	}
	return true, nil
}

// rewriteProviderArtifactsTranscriptPath replaces a leaked transcriptPath under
// providerState.artifacts when present. The nested map is decoded, mutated, and
// re-encoded so untouched fields and ordering siblings are preserved.
func rewriteProviderArtifactsTranscriptPath(metadataPath string, fields map[string]json.RawMessage, canonical string) (bool, error) {
	providerStateRaw, ok := fields[providerStateKey]
	if !ok || len(providerStateRaw) == 0 {
		return false, nil
	}
	providerState := make(map[string]json.RawMessage)
	if err := json.Unmarshal(providerStateRaw, &providerState); err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_provider_state_decode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("decode provider state: %w", err)
	}
	artifactsRaw, ok := providerState[artifactsKey]
	if !ok || len(artifactsRaw) == 0 {
		return false, nil
	}
	artifacts := make(map[string]json.RawMessage)
	if err := json.Unmarshal(artifactsRaw, &artifacts); err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_artifacts_decode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("decode artifacts: %w", err)
	}
	currentNested := decodeStringField(artifacts, transcriptPathKey)
	if !shouldRewriteTranscriptPath(currentNested, canonical) {
		return false, nil
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_encode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"field", "providerState.artifacts.transcriptPath",
			"err", err,
		)
		return false, fmt.Errorf("encode canonical transcript path for artifacts: %w", err)
	}
	artifacts[transcriptPathKey] = encoded
	encodedArtifacts, err := json.Marshal(artifacts)
	if err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_artifacts_encode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("encode artifacts map: %w", err)
	}
	providerState[artifactsKey] = encodedArtifacts
	encodedProviderState, err := json.Marshal(providerState)
	if err != nil {
		sessionLog.Warn("session.metadata.transcript_path_migration_provider_state_encode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("encode provider state: %w", err)
	}
	fields[providerStateKey] = encodedProviderState
	return true, nil
}

// shouldRewriteTranscriptPath decides whether to swap in the canonical path.
// A current value that is empty, equals the canonical, or already points at a
// real on-disk file outside any rotator-launch directory is left as-is.
func shouldRewriteTranscriptPath(current, canonical string) bool {
	current = strings.TrimSpace(current)
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return false
	}
	if current == "" {
		return false
	}
	if current == canonical {
		return false
	}
	if strings.Contains(current, rotatorLaunchPathSegment) {
		return true
	}
	if _, err := os.Stat(current); err != nil {
		return true
	}
	return false
}

func decodeStringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func logTranscriptPathMigrationResult(result TranscriptPathMigrationResult) {
	sessionLog.Info(migrationTranscriptPathEventName,
		"component", migrationComponent,
		"subcomponent", migrationSubcomponent,
		"scanned", result.Scanned,
		"migrated", result.Migrated,
		"skipped", result.Skipped,
		"failed", result.Failed,
	)
}
