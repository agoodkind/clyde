package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	virtualPathPrefix = "cursor://"
	rootHashHexLength = 16

	// VirtualKindComposer identifies SQLite-backed Cursor composer artifacts.
	VirtualKindComposer = "composer"
	// VirtualKindLegacy identifies SQLite-backed Cursor legacy chat artifacts.
	VirtualKindLegacy = "legacy"
)

// CursorVirtualPath names one stable virtual Cursor artifact path.
type CursorVirtualPath struct {
	RootHash string
	Kind     string
	ID       string
}

// RootHash returns the stable short hash Clyde uses to identify one Cursor data
// root inside virtual artifact paths.
func RootHash(rootDir string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rootDir))
	return hex.EncodeToString(sum[:8])
}

// BuildVirtualPath builds a stable virtual path for one Cursor SQLite artifact.
func BuildVirtualPath(rootHash string, kind string, id string) string {
	if !safeVirtualPart(rootHash) || !safeVirtualPart(kind) || !safeVirtualPart(id) {
		return ""
	}
	path := virtualPathPrefix + rootHash + "/" + kind + "/" + id
	if _, err := ParseVirtualPath(path); err != nil {
		return ""
	}
	return path
}

// ParseVirtualPath decodes one Cursor virtual artifact path back into its root
// hash, kind, and artifact id components.
func ParseVirtualPath(raw string) (CursorVirtualPath, error) {
	if !strings.HasPrefix(raw, virtualPathPrefix) {
		return CursorVirtualPath{}, fmt.Errorf("invalid cursor virtual path %q", raw)
	}
	trimmed := strings.TrimPrefix(raw, virtualPathPrefix)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 3 {
		return CursorVirtualPath{}, fmt.Errorf("invalid cursor virtual path %q", raw)
	}
	if !safeVirtualPart(parts[0]) || !safeVirtualPart(parts[1]) || !safeVirtualPart(parts[2]) {
		return CursorVirtualPath{}, fmt.Errorf("invalid cursor virtual path %q", raw)
	}
	if len(parts[0]) != rootHashHexLength {
		return CursorVirtualPath{}, fmt.Errorf("invalid cursor virtual path %q", raw)
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return CursorVirtualPath{}, fmt.Errorf("invalid cursor virtual path %q", raw)
	}
	return CursorVirtualPath{
		RootHash: parts[0],
		Kind:     parts[1],
		ID:       parts[2],
	}, nil
}

func legacyID(workspaceHash string, tabID string) string {
	if workspaceHash == "" || tabID == "" {
		return ""
	}
	return workspaceHash + "~" + tabID
}

func splitLegacyID(id string) (string, string, bool) {
	workspaceHash, tabID, found := strings.Cut(id, "~")
	if !found {
		return "", "", false
	}
	if workspaceHash == "" || tabID == "" {
		return "", "", false
	}
	if strings.Contains(tabID, "~") {
		return "", "", false
	}
	return workspaceHash, tabID, true
}

func safeVirtualPart(part string) bool {
	if part == "" {
		return false
	}
	if part != strings.TrimSpace(part) {
		return false
	}
	return !strings.ContainsAny(part, "/?#")
}
