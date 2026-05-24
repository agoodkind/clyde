package claude

import (
	"net"
	"net/url"
	"strings"
)

const (
	// AnthropicBaseURLEnv is the Claude CLI override Clyde uses for MITM capture.
	AnthropicBaseURLEnv = "ANTHROPIC_BASE_URL"
	// ClydeMITMAnthropicBaseURLEnv marks AnthropicBaseURLEnv as Clyde-owned.
	ClydeMITMAnthropicBaseURLEnv = "CLYDE_MITM_ANTHROPIC_BASE_URL"
	// ClaudeConfigDirEnv is the Claude CLI config-dir override. When Clyde
	// routes a launched claude through the OAuth rotator (CLYDE-448) the daemon
	// plants the selected account's credentials in a scratch dir and sets this
	// to that dir; on macOS the keychain entry is empty for a non-default config
	// dir, so claude reads the planted .credentials.json there.
	ClaudeConfigDirEnv = "CLAUDE_CONFIG_DIR"
	// ClydeLaunchClaudeConfigDirEnv marks ClaudeConfigDirEnv as Clyde-owned so a
	// stale value inherited from a prior Clyde launch is scrubbed before a fresh
	// daemon-supplied value (or no value) is applied on top.
	ClydeLaunchClaudeConfigDirEnv = "CLYDE_LAUNCH_CLAUDE_CONFIG_DIR"
)

// SanitizeMITMMap removes stale Clyde-owned MITM environment from a mutable env
// map. It also scrubs a Clyde-owned CLAUDE_CONFIG_DIR so a stale planted-config
// dir from a prior launch cannot leak into a new one.
func SanitizeMITMMap(env map[string]string) {
	baseURL := strings.TrimSpace(env[AnthropicBaseURLEnv])
	marker := strings.TrimSpace(env[ClydeMITMAnthropicBaseURLEnv])
	if marker == "1" || isLoopbackHTTPBaseURL(baseURL) {
		delete(env, AnthropicBaseURLEnv)
	}
	delete(env, ClydeMITMAnthropicBaseURLEnv)
	if strings.TrimSpace(env[ClydeLaunchClaudeConfigDirEnv]) == "1" {
		delete(env, ClaudeConfigDirEnv)
	}
	delete(env, ClydeLaunchClaudeConfigDirEnv)
}

// ApplyMITMMap marks and installs a daemon-provided Anthropic MITM base URL.
func ApplyMITMMap(env map[string]string, baseURL string) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return
	}
	env[AnthropicBaseURLEnv] = baseURL
	env[ClydeMITMAnthropicBaseURLEnv] = "1"
}

// ApplyClaudeConfigDirMap marks and installs a daemon-provided CLAUDE_CONFIG_DIR
// so the launched claude reads the rotator-planted credentials. The marker lets
// a later SanitizeMITMMap scrub this value if the env is reused for another
// launch.
func ApplyClaudeConfigDirMap(env map[string]string, configDir string) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return
	}
	env[ClaudeConfigDirEnv] = configDir
	env[ClydeLaunchClaudeConfigDirEnv] = "1"
}

// SanitizeMITMList removes stale Clyde-owned MITM environment from an env list.
// It also scrubs a Clyde-owned CLAUDE_CONFIG_DIR so a stale planted-config dir
// from a prior launch cannot leak when an inherited env is forwarded to a child.
func SanitizeMITMList(env []string) []string {
	baseURL := envValue(env, AnthropicBaseURLEnv)
	marker := envValue(env, ClydeMITMAnthropicBaseURLEnv)
	removeBaseURL := marker == "1" || isLoopbackHTTPBaseURL(baseURL)
	removeConfigDir := envValue(env, ClydeLaunchClaudeConfigDirEnv) == "1"
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if key == ClydeMITMAnthropicBaseURLEnv || key == ClydeLaunchClaudeConfigDirEnv {
			continue
		}
		if key == AnthropicBaseURLEnv && removeBaseURL {
			continue
		}
		if key == ClaudeConfigDirEnv && removeConfigDir {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isLoopbackHTTPBaseURL(rawValue string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawValue))
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
