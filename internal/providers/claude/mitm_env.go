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
)

func SanitizeMITMMap(env map[string]string) {
	baseURL := strings.TrimSpace(env[AnthropicBaseURLEnv])
	marker := strings.TrimSpace(env[ClydeMITMAnthropicBaseURLEnv])
	if marker == "1" || isLoopbackHTTPBaseURL(baseURL) {
		delete(env, AnthropicBaseURLEnv)
	}
	delete(env, ClydeMITMAnthropicBaseURLEnv)
}

func ApplyMITMMap(env map[string]string, baseURL string) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return
	}
	env[AnthropicBaseURLEnv] = baseURL
	env[ClydeMITMAnthropicBaseURLEnv] = "1"
}

func SanitizeMITMList(env []string) []string {
	baseURL := envValue(env, AnthropicBaseURLEnv)
	marker := envValue(env, ClydeMITMAnthropicBaseURLEnv)
	removeBaseURL := marker == "1" || isLoopbackHTTPBaseURL(baseURL)
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if key == ClydeMITMAnthropicBaseURLEnv {
			continue
		}
		if key == AnthropicBaseURLEnv && removeBaseURL {
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
