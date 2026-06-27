package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const virtualPathPrefix = "zed://"

// VirtualPath is the parsed shape of one synthetic Zed conversation path.
type VirtualPath struct {
	RootHash  string
	Channel   string
	SessionID string
}

// BuildVirtualPath returns the stable synthetic path Clyde uses for one native
// Zed thread.
func BuildVirtualPath(rootDir, channel, sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rootDir)))
	rootHash := hex.EncodeToString(sum[:8])
	path := virtualPathPrefix + rootHash + "/" + strings.TrimSpace(channel) + "/" + strings.TrimSpace(sessionID)
	if _, err := ParseVirtualPath(path); err != nil {
		return ""
	}
	return path
}

// ParseVirtualPath decodes one synthetic Zed conversation path.
func ParseVirtualPath(raw string) (VirtualPath, error) {
	if !strings.HasPrefix(raw, virtualPathPrefix) {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	trimmed := strings.TrimPrefix(raw, virtualPathPrefix)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 3 {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	path := VirtualPath{
		RootHash:  strings.TrimSpace(parts[0]),
		Channel:   strings.TrimSpace(parts[1]),
		SessionID: strings.TrimSpace(parts[2]),
	}
	if path.RootHash == "" || path.Channel == "" || path.SessionID == "" {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	return path, nil
}
