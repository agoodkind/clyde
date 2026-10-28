package slogger_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/slogger"
)

func TestCleanupAccountsForUnlinkedOpenLogs(t *testing.T) {
	for _, retainedMegabytes := range []int{1, 6} {
		t.Run(fmt.Sprintf("%d MiB open", retainedMegabytes), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "unlinked.jsonl")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			line := []byte("{\"event\":\"retained\"}\n")
			data := bytes.Repeat(line, retainedMegabytes*1024*1024/len(line))
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			const budget = 5 * 1024 * 1024
			var namedBytes int64
			for _, name := range []string{"older.jsonl", "newer.jsonl"} {
				named := bytes.Repeat(line, 4*1024*1024/len(line))
				if err := os.WriteFile(filepath.Join(root, name), named, 0o600); err != nil {
					t.Fatal(err)
				}
				namedBytes += int64(len(named))
			}
			policy := slogger.CleanupPolicy{Enabled: true, Root: root, MaxTotalMB: 5, RetainedOpenBytes: info.Size(), RetainedOpenFiles: 1}
			result, cleanupError := slogger.RunCleanupOnce(policy)
			if result.BytesBefore != namedBytes || result.RetainedOpenBytes != info.Size() || result.RetainedOpenFiles != 1 {
				t.Fatalf("incorrect named and unlinked byte counts: %+v", result)
			}
			available := int64(budget)
			if info.Size() < budget {
				available -= info.Size()
			}
			if result.BytesAfter <= 0 || result.BytesAfter > available || result.BytesTrimmed <= 0 {
				t.Fatalf("cleanup did not retain a bounded set of named logs: %+v", result)
			}
			wantExcess := max(int64(0), result.BytesAfter+info.Size()-budget)
			if result.ExcessBytes != wantExcess || (cleanupError != nil) != (wantExcess > 0) {
				t.Fatalf("incorrect unavoidable excess: result=%+v error=%v", result, cleanupError)
			}
			if result.BytesDeleted+result.BytesTrimmed != result.BytesBefore-result.BytesAfter {
				t.Fatalf("cleanup counted unlinked bytes as reclaimed: %+v", result)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			policy.RetainedOpenBytes = 0
			policy.RetainedOpenFiles = 0
			afterClose, err := slogger.RunCleanupOnce(policy)
			if err != nil || afterClose.ExcessBytes != 0 || afterClose.RetainedOpenBytes != 0 {
				t.Fatalf("cleanup retained an excess after the descriptor closed: result=%+v error=%v", afterClose, err)
			}
		})
	}
}
