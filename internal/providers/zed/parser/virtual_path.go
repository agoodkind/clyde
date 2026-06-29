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

// VirtualPath names one stable virtual Zed artifact path.
type VirtualPath struct {
	RootHash  string
	Channel   string
	SessionID string
}

// RootHash returns the stable short hash Clyde uses to identify one Zed data
// root inside virtual artifact paths.
func RootHash(rootDir string) string {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rootDir))
	return hex.EncodeToString(sum[:8])
}

// ParseVirtualPath decodes one Zed virtual artifact path back into its root
// hash, channel, and session ID components.
func ParseVirtualPath(raw string) (VirtualPath, error) {
	if !strings.HasPrefix(raw, virtualPathPrefix) {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	trimmed := strings.TrimPrefix(raw, virtualPathPrefix)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 3 {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	if parts[0] != strings.TrimSpace(parts[0]) || parts[1] != strings.TrimSpace(parts[1]) || parts[2] != strings.TrimSpace(parts[2]) {
		return VirtualPath{}, fmt.Errorf("invalid zed virtual path %q", raw)
	}
	path := VirtualPath{
		RootHash:  parts[0],
		Channel:   parts[1],
		SessionID: parts[2],
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

func buildVirtualPathFromRootHash(rootHash, channel, sessionID string) string {
	if rootHash == "" || channel == "" || sessionID == "" {
		return ""
	}
	if strings.Contains(channel, "/") || strings.Contains(sessionID, "/") {
		return ""
	}
	path := virtualPathPrefix + rootHash + "/" + channel + "/" + sessionID
	if _, err := ParseVirtualPath(path); err != nil {
		return ""
	}
	return path
}
