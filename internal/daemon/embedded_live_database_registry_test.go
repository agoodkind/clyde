//go:build live

package daemon

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func registerLiveMilvusDatabase(t *testing.T, database, address string) {
	t.Helper()
	path := os.Getenv("CLYDE_LIVE_DATABASE_REGISTRY")
	if path == "" {
		return
	}
	directory := filepath.Dir(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "database-registration.jsonl" || filepath.Dir(directory) != "/private/tmp" || !strings.HasPrefix(filepath.Base(directory), "clyde-alias-live-") {
		t.Fatal("database registry requires an absolute private alias fixture path")
	}
	if address != "localhost:39530" {
		t.Fatalf("database registry rejects endpoint %s", address)
	}
	suffix := strings.TrimPrefix(database, "clyde_live_")
	if suffix == database {
		suffix = strings.TrimPrefix(database, "clyde_query_")
	}
	decoded, err := hex.DecodeString(suffix)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("database registry rejects generated name %s", database)
	}
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database registry directory is not private: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database registry file is not private and regular: %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open existing database registry: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close database registry: %v", err)
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		t.Fatalf("database registry changed before append: %v", err)
	}
	entry := struct {
		Database        string `json:"database"`
		Address         string `json:"address"`
		AbsenceVerified bool   `json:"absence_verified"`
	}{Database: database, Address: address, AbsenceVerified: true}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		t.Fatalf("append database registry: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync database registry: %v", err)
	}
	t.Logf("registered absent isolated database %s at %s before creation", database, address)
}
