package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	virtualPathPrefix = "zed://"
	rootHashHexLength = 16
)

// VirtualPath is the parsed shape of one synthetic Zed conversation path.
type VirtualPath struct {
	RootHash  string
	Channel   string
	SessionID string
}

// BuildVirtualPath returns the stable synthetic path Clyde uses for one native
// Zed thread.
func BuildVirtualPath(rootDir, channel, sessionID string) string {
	channel = strings.TrimSpace(channel)
	sessionID = strings.TrimSpace(sessionID)
	if channel == "" || sessionID == "" {
		return ""
	}
	if strings.Contains(channel, "/") || strings.Contains(sessionID, "/") {
		return ""
	}
	path := virtualPathPrefix + RootHash(rootDir) + "/" + channel + "/" + sessionID
	if _, err := ParseVirtualPath(path); err != nil {
		return ""
	}
	return path
}

// RootHash returns the stable short hash Clyde uses to identify one Zed data
// root inside virtual artifact paths.
func RootHash(rootDir string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rootDir)))
	return hex.EncodeToString(sum[:8])
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
	if len(path.RootHash) != rootHashHexLength {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	if _, err := hex.DecodeString(path.RootHash); err != nil {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	if strings.ContainsAny(path.Channel, "?#") || strings.ContainsAny(path.SessionID, "?#") {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	return path, nil
}
