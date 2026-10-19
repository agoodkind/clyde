package codexstore_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

const (
	compressedTestFilename = "rollout-2026-05-02T10-09-00-compressed-thread.jsonl"
	compressedTestHeader   = `{"timestamp":"2026-05-02T17:09:00Z","type":"session_meta","payload":{"id":"compressed-thread","timestamp":"2026-05-02T17:09:00Z","cwd":"/repo","source":"cli","model_provider":"openai"}}` + "\n"
	compressedTestMessage  = `{"timestamp":"2026-05-02T17:09:01Z","type":"event_msg","payload":{"type":"user_message","message":"read compressed history"}}` + "\n"
)

func compressRolloutFixture(t *testing.T, body string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func writeCompressedFixture(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, compressRolloutFixture(t, body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedRolloutHeaderAndMessagesMatchPlain(t *testing.T) {
	t.Parallel()
	plainPath := filepath.Join(t.TempDir(), compressedTestFilename)
	compressedPath := filepath.Join(t.TempDir(), compressedTestFilename+".zst")
	body := compressedTestHeader + compressedTestMessage + `{"type":"compacted","payload":{"message":"summary","replacement_history":[]}}` + "\n"
	if err := os.WriteFile(plainPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCompressedFixture(t, compressedPath, body)
	plainHeader, err := codexstore.ReadHeader(plainPath, true)
	if err != nil {
		t.Fatal(err)
	}
	compressedHeader, err := codexstore.ReadHeader(compressedPath, true)
	if err != nil {
		t.Fatal(err)
	}
	compressedHeader.RolloutPath = plainHeader.RolloutPath
	if !reflect.DeepEqual(plainHeader, compressedHeader) {
		t.Fatalf("compressed header = %+v, want %+v", compressedHeader, plainHeader)
	}
	for _, includeSystem := range []bool{false, true} {
		plain := collectCompressedHistoryMessages(t, plainPath, includeSystem)
		compressed := collectCompressedHistoryMessages(t, compressedPath, includeSystem)
		if !reflect.DeepEqual(plain, compressed) {
			t.Fatalf("compressed messages = %+v, want %+v", compressed, plain)
		}
	}
}

func TestCompressedRolloutDiscoveryPrefersPlainSibling(t *testing.T) {
	t.Parallel()
	paths, err := codexstore.ResolveStorePaths(t.Context(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.SessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plainPath := filepath.Join(paths.SessionsDir, compressedTestFilename)
	compressedPath := plainPath + ".zst"
	writeCompressedFixture(t, compressedPath, compressedTestHeader+compressedTestMessage)
	for _, name := range []string{compressedTestFilename, compressedTestFilename + ".zst"} {
		id, _, ok := codexstore.RolloutIdentityFromFilename(name)
		if !ok || id != "compressed-thread" {
			t.Fatalf("filename identity for %q = %q, %v", name, id, ok)
		}
	}
	for _, expectedPath := range []string{compressedPath, plainPath} {
		if expectedPath == plainPath {
			if err := os.WriteFile(plainPath, []byte(compressedTestHeader+compressedTestMessage), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		candidates, err := codexstore.NewDiscoveryScanner(paths).DiscoverCandidates()
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].Path != expectedPath {
			t.Fatalf("candidates = %+v, want one at %s", candidates, expectedPath)
		}
	}
}

func TestCompressedRolloutReadErrors(t *testing.T) {
	t.Parallel()
	compressed := compressRolloutFixture(t, compressedTestHeader+compressedTestMessage)
	corrupt := bytes.Clone(compressed)
	corrupt[len(corrupt)-1] ^= 0xff
	for name, data := range map[string][]byte{
		"invalid_header":      []byte("invalid zstd frame"),
		"truncated_header":    compressed[:3],
		"truncated_body":      compressed[:len(compressed)/2],
		"truncated_checksum":  compressed[:len(compressed)-1],
		"checksum_corruption": corrupt,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), compressedTestFilename+".zst")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var streamErr error
			for _, err := range codexstore.StreamMessages(path, codexstore.HistoryOptions{}) {
				if err != nil {
					streamErr = err
					break
				}
			}
			if streamErr == nil {
				t.Fatal("corrupt compressed rollout did not fail")
			}
			if name == "invalid_header" || name == "truncated_header" || name == "truncated_body" {
				if _, err := codexstore.ReadHeader(path, false); err == nil {
					t.Fatal("corrupt compressed header did not fail")
				}
			}
		})
	}
}

func rolloutOpenDescriptors(t *testing.T, path string) int {
	t.Helper()
	var info unix.Stat_t
	err := unix.Stat(path, &info)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		descriptor, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		var descriptorInfo unix.Stat_t
		statErr := unix.Fstat(descriptor, &descriptorInfo)
		if statErr == nil && info.Dev == descriptorInfo.Dev && info.Ino == descriptorInfo.Ino {
			count++
		}
	}
	return count
}

func TestCompressedRolloutEarlyStopClosesSource(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), compressedTestFilename+".zst")
	data := compressRolloutFixture(t, compressedTestHeader+strings.Repeat(compressedTestMessage, 1000))
	data = append(data, []byte("corrupt trailing frame")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := codexstore.ReadHeader(path, false); err != nil {
		t.Fatalf("bounded header read: %v", err)
	}
	if count := rolloutOpenDescriptors(t, path); count != 0 {
		t.Fatalf("open source descriptors after header = %d", count)
	}
	yielded := 0
	for _, err := range codexstore.StreamMessages(path, codexstore.HistoryOptions{}) {
		if err != nil {
			t.Fatalf("early stream read: %v", err)
		}
		yielded++
		if count := rolloutOpenDescriptors(t, path); count != 1 {
			t.Fatalf("open source descriptors during stream = %d, want 1", count)
		}
		break
	}
	if yielded != 1 {
		t.Fatalf("yielded = %d, want 1", yielded)
	}
	if count := rolloutOpenDescriptors(t, path); count != 0 {
		t.Fatalf("open source descriptors after early stop = %d", count)
	}
	var streamErr error
	for _, err := range codexstore.StreamMessages(path, codexstore.HistoryOptions{}) {
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil {
		t.Fatal("full read did not report trailing frame corruption")
	}
}

func collectCompressedHistoryMessages(t *testing.T, path string, includeSystem bool) []codexstore.HistoryMessage {
	t.Helper()
	var messages []codexstore.HistoryMessage
	for message, err := range codexstore.StreamMessages(path, codexstore.HistoryOptions{IncludeSystemMessages: includeSystem}) {
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}
