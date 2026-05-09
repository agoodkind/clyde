// Package codex owns Codex-specific provider helpers outside the lifecycle
// package.
package codex

import (
	"net"
	"net/url"
	"slices"
	"strings"
)

const (
	// OpenAIBaseURLEnv is the Codex CLI override Clyde can use for OpenAI-style
	// HTTP routing.
	OpenAIBaseURLEnv = "OPENAI_BASE_URL"
	// SSLCertFileEnv points TLS clients at Clyde's MITM CA certificate bundle.
	SSLCertFileEnv = "SSL_CERT_FILE"
	// NodeExtraCACertsEnv points Node-based TLS clients at Clyde's MITM CA.
	NodeExtraCACertsEnv = "NODE_EXTRA_CA_CERTS"
	// HTTPProxyEnv is the uppercase HTTP proxy override Clyde sets for Codex.
	HTTPProxyEnv = "HTTP_PROXY"
	// HTTPSProxyEnv is the uppercase HTTPS proxy override Clyde sets for Codex.
	HTTPSProxyEnv = "HTTPS_PROXY"
	// AllProxyEnv is the uppercase catch-all proxy override Clyde sets for Codex.
	AllProxyEnv        = "ALL_PROXY"
	lowerHTTPProxyEnv  = "http_proxy"
	lowerHTTPSProxyEnv = "https_proxy"
	lowerAllProxyEnv   = "all_proxy"
	// ClydeOpenAIBaseURLEnv marks OpenAIBaseURLEnv as Clyde-owned.
	ClydeOpenAIBaseURLEnv = "CLYDE_OPENAI_BASE_URL"
	// ClydeMITMProxyEnv marks proxy overrides as Clyde-owned.
	ClydeMITMProxyEnv = "CLYDE_MITM_PROXY"
	// ClydeMITMCertEnv marks TLS certificate overrides as Clyde-owned.
	ClydeMITMCertEnv = "CLYDE_MITM_CERT_PATH"
)

var codexProxyEnvKeys = []string{
	HTTPProxyEnv,
	HTTPSProxyEnv,
	AllProxyEnv,
	lowerHTTPProxyEnv,
	lowerHTTPSProxyEnv,
	lowerAllProxyEnv,
}

var codexCertEnvKeys = []string{
	SSLCertFileEnv,
	NodeExtraCACertsEnv,
}

// SanitizeLaunchList removes stale Clyde-owned Codex launch environment.
func SanitizeLaunchList(env []string) []string {
	removeBaseURL := envValue(env, ClydeOpenAIBaseURLEnv) == "1" ||
		isLoopbackHTTPBaseURL(envValue(env, OpenAIBaseURLEnv))
	removeProxy := envValue(env, ClydeMITMProxyEnv) == "1" || listHasLoopbackProxy(env)
	removeCert := envValue(env, ClydeMITMCertEnv) == "1"
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if key == ClydeOpenAIBaseURLEnv || key == ClydeMITMProxyEnv || key == ClydeMITMCertEnv {
			continue
		}
		if removeBaseURL && key == OpenAIBaseURLEnv {
			continue
		}
		if removeProxy && isCodexProxyEnvKey(key) {
			continue
		}
		if removeCert && isCodexCertEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ApplyLaunchList marks and installs daemon-provided Codex launch environment.
func ApplyLaunchList(env []string, baseURL, proxyURL, caCertPath string) []string {
	out := append([]string(nil), env...)
	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		out = withEnvValue(out, OpenAIBaseURLEnv, baseURL)
		out = withEnvValue(out, ClydeOpenAIBaseURLEnv, "1")
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		for _, key := range codexProxyEnvKeys {
			out = withEnvValue(out, key, proxyURL)
		}
		out = withEnvValue(out, ClydeMITMProxyEnv, "1")
	}
	caCertPath = strings.TrimSpace(caCertPath)
	if caCertPath != "" {
		for _, key := range codexCertEnvKeys {
			out = withEnvValue(out, key, caCertPath)
		}
		out = withEnvValue(out, ClydeMITMCertEnv, "1")
	}
	return out
}

// IsMITMProxyKey reports whether key is one of Clyde's Codex proxy override
// variables.
func IsMITMProxyKey(key string) bool {
	return isCodexProxyEnvKey(strings.TrimSpace(key))
}

func isCodexProxyEnvKey(key string) bool {
	return slices.Contains(codexProxyEnvKeys, key)
}

func isCodexCertEnvKey(key string) bool {
	return slices.Contains(codexCertEnvKeys, key)
}

func listHasLoopbackProxy(env []string) bool {
	for _, key := range codexProxyEnvKeys {
		if isLoopbackHTTPProxyURL(envValue(env, key)) {
			return true
		}
	}
	return false
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

func isLoopbackHTTPProxyURL(rawValue string) bool {
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

func withEnvValue(env []string, key, value string) []string {
	if key == "" || value == "" {
		return env
	}
	prefix := key + "="
	out := append([]string(nil), env...)
	for i, item := range out {
		if strings.HasPrefix(item, prefix) {
			out[i] = prefix + value
			return out
		}
	}
	return append(out, prefix+value)
}
