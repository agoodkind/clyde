package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/util"
)

const (
	metadataTitleKey        = "title"
	metadataLegacyTitleKey  = "displayTitle"
	migrationComponent      = "session"
	migrationSubcomponent   = "migration"
	migrationTitleEventName = "session.metadata.title_migration_completed"
)

// TitleMetadataMigrationResult reports the outcome of the title-key migration.
type TitleMetadataMigrationResult struct {
	Scanned  int
	Migrated int
	Skipped  int
	Failed   int
}

// MigrateTitleMetadata rewrites legacy metadata title keys before sessions are read.
func MigrateTitleMetadata(clydeRoot string) (TitleMetadataMigrationResult, error) {
	sessionsDir := config.GetSessionsDir(clydeRoot)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			result := TitleMetadataMigrationResult{
				Scanned:  0,
				Migrated: 0,
				Skipped:  0,
				Failed:   0,
			}
			logTitleMigrationResult(result)
			return result, nil
		}
		sessionLog.Warn("session.metadata.title_migration_readdir_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", sessionsDir,
			"err", err,
		)
		return TitleMetadataMigrationResult{
			Scanned:  0,
			Migrated: 0,
			Skipped:  0,
			Failed:   0,
		}, fmt.Errorf("read sessions directory for title migration: %w", err)
	}

	result := TitleMetadataMigrationResult{
		Scanned:  0,
		Migrated: 0,
		Skipped:  0,
		Failed:   0,
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		result.Scanned++
		metadataPath := filepath.Join(sessionsDir, entry.Name(), metadataFile)
		migrated, migrateErr := migrateTitleMetadataFile(metadataPath)
		if migrateErr != nil {
			result.Failed++
			sessionLog.Warn("session.metadata.title_migration_file_failed",
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
	logTitleMigrationResult(result)
	return result, nil
}

func migrateTitleMetadataFile(metadataPath string) (bool, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		sessionLog.Warn("session.metadata.title_migration_read_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("read metadata file: %w", err)
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &fields); err != nil {
		sessionLog.Warn("session.metadata.title_migration_decode_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("decode metadata file: %w", err)
	}
	legacyTitle, hasLegacyTitle := fields[metadataLegacyTitleKey]
	if !hasLegacyTitle {
		return false, nil
	}
	if _, hasTitle := fields[metadataTitleKey]; !hasTitle {
		fields[metadataTitleKey] = legacyTitle
	}
	delete(fields, metadataLegacyTitleKey)
	if err := util.WriteJSON(metadataPath, fields); err != nil {
		sessionLog.Warn("session.metadata.title_migration_write_failed",
			"component", migrationComponent,
			"subcomponent", migrationSubcomponent,
			"path", metadataPath,
			"err", err,
		)
		return false, fmt.Errorf("write migrated metadata file: %w", err)
	}
	return true, nil
}

func logTitleMigrationResult(result TitleMetadataMigrationResult) {
	sessionLog.Info(migrationTitleEventName,
		"component", migrationComponent,
		"subcomponent", migrationSubcomponent,
		"scanned", result.Scanned,
		"migrated", result.Migrated,
		"skipped", result.Skipped,
		"failed", result.Failed,
	)
}
