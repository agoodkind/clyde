package mitm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

func readCaptureRecordsRaw(path string, providerFilter string) ([][]byte, []CaptureRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("mitm.capture.open_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return nil, nil, fmt.Errorf("open capture records %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var rawLines [][]byte
	var records []CaptureRecord
	wantProvider := strings.TrimSpace(providerFilter)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec CaptureRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if wantProvider != "" && !strings.EqualFold(strings.TrimSpace(rec.Provider), wantProvider) {
			continue
		}
		copied := make([]byte, len(line))
		copy(copied, line)
		rawLines = append(rawLines, copied)
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("scan capture records %s: %w", path, err)
	}
	return rawLines, records, nil
}
