package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
)

const (
	retainedDirName       = "conversation"
	retainedFilename      = "retained-records.json"
	retainedFormatVersion = 1
	retainedDirMode       = 0o700
	retainedFileMode      = 0o600
	retainedTempPattern   = ".retained-records-*.tmp"
)

// RetainedPath returns the path of the retained conversation records file in the Clyde state directory.
func RetainedPath() string {
	return filepath.Join(config.DefaultStateDir(), retainedDirName, retainedFilename)
}

type retainedFile struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// readRetained returns records from path. If the file does not exist, it returns an empty set. It returns an error if it cannot read or decode the file or the version is unsupported.
func readRetained(path string) ([]Record, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		slog.Warn("conversation.retained.read_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("read retained records file %s: %w", path, err)
	}
	var file retainedFile
	if err := json.Unmarshal(content, &file); err != nil {
		slog.Warn("conversation.retained.decode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("decode retained records file %s: %w", path, err)
	}
	if file.Version != retainedFormatVersion {
		err := fmt.Errorf("retained records file %s has unsupported format version %d; supported version is %d", path, file.Version, retainedFormatVersion)
		slog.Warn("conversation.retained.version_rejected", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, err
	}
	return file.Records, nil
}

func writeRetained(path string, records []Record) (failure error) {
	defer func() {
		if failure != nil {
			slog.Warn("conversation.retained.write_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", failure)
			return
		}
		slog.Info("conversation.retained.written", "concern", "conversation.index", "component", "conversation", "path", path, "records", len(records))
	}()
	if records == nil {
		records = []Record{}
	}
	content, err := json.Marshal(retainedFile{Version: retainedFormatVersion, Records: records})
	if err != nil {
		return fmt.Errorf("encode retained records: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, retainedDirMode); err != nil {
		return fmt.Errorf("create retained records directory %s: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, retainedTempPattern)
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", directory, err)
	}
	tempPath := temp.Name()
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write temporary retained records file %s: %w", tempPath, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("sync temporary retained records file %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close temporary retained records file %s: %w", tempPath, err)
	}
	if err := os.Chmod(tempPath, retainedFileMode); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("set mode of temporary retained records file %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace retained records file %s: %w", path, err)
	}
	return nil
}

// retainDropped returns retained records, the new record count, and whether the set changed. It adds previous records absent from the scan and removes retained records found again.
func retainDropped(previous []Record, current []Record, retained []Record) ([]Record, int, bool) {
	present := make(map[string]bool, len(current))
	for _, record := range current {
		present[record.ID] = true
	}
	next := make([]Record, 0, len(retained))
	kept := make(map[string]bool, len(retained))
	changed := false
	for _, record := range retained {
		if present[record.ID] {
			changed = true
			continue
		}
		next = append(next, record)
		kept[record.ID] = true
	}
	added := 0
	for _, record := range previous {
		if record.ID == "" || present[record.ID] || kept[record.ID] {
			continue
		}
		next = append(next, record)
		kept[record.ID] = true
		added++
		changed = true
	}
	sortRecords(next)
	return next, added, changed
}
