// Package providerid defines Clyde's provider enum at the generic boundary.
package providerid

import (
	"strconv"
	"strings"
)

// Provider identifies a raw tool or adapter provider without carrying
// provider-specific process semantics.
type Provider uint8

const (
	// ProviderUnspecified is the zero value.
	ProviderUnspecified Provider = iota
	// ProviderClaude identifies Claude transcript artifacts.
	ProviderClaude
	// ProviderCodex identifies Codex rollout artifacts.
	ProviderCodex
	// ProviderAnthropic identifies direct Anthropic adapter traffic.
	ProviderAnthropic
	// ProviderOpenAICompat identifies OpenAI-compatible adapter traffic.
	ProviderOpenAICompat
	// ProviderMITM identifies MITM proxy traffic.
	ProviderMITM
	// ProviderArtifact identifies an artifact whose native provider is unknown.
	ProviderArtifact
	// ProviderCursor identifies Cursor traffic and conversation artifacts.
	ProviderCursor
	// ProviderConductor identifies Conductor MITM traffic.
	ProviderConductor
	// ProviderZed identifies Zed thread artifacts.
	ProviderZed
)

type providerLabel string

const (
	labelUnspecified          providerLabel = ""
	labelUnspecifiedText      providerLabel = "unspecified"
	labelClaude               providerLabel = "claude"
	labelClaudeCLI            providerLabel = "claude-cli"
	labelCodex                providerLabel = "codex"
	labelOpenAICodex          providerLabel = "openai-codex"
	labelOpenAICodexApp       providerLabel = "openai-codex-app"
	labelAnthropic            providerLabel = "anthropic"
	labelAnthropicOAuth       providerLabel = "anthropic-oauth"
	labelOpenAICompat         providerLabel = "openai_compat"
	labelOpenAICompatHyphen   providerLabel = "openai-compat"
	labelOpenAICompatForward  providerLabel = "openai-compat-passthrough"
	labelMITM                 providerLabel = "mitm"
	labelArtifact             providerLabel = "artifact"
	labelCursor               providerLabel = "cursor"
	labelConductor            providerLabel = "conductor"
	labelZed                  providerLabel = "zed"
	labelZedAgent             providerLabel = "zed-agent"
	labelZedAgentUnderscore   providerLabel = "zed_agent"
	passthroughOverridePrefix               = "passthrough-override-"
)

// Parse converts a wire label into a Provider enum.
func Parse(raw string) (Provider, bool) {
	label := providerLabel(strings.TrimSpace(strings.ToLower(raw)))
	switch label {
	case labelUnspecified:
		return ProviderUnspecified, false
	case labelUnspecifiedText:
		return ProviderUnspecified, true
	case labelClaude, labelClaudeCLI:
		return ProviderClaude, true
	case labelCodex, labelOpenAICodex, labelOpenAICodexApp:
		return ProviderCodex, true
	case labelAnthropic, labelAnthropicOAuth:
		return ProviderAnthropic, true
	case labelOpenAICompat, labelOpenAICompatHyphen, labelOpenAICompatForward:
		return ProviderOpenAICompat, true
	case labelMITM:
		return ProviderMITM, true
	case labelArtifact:
		return ProviderArtifact, true
	case labelCursor:
		return ProviderCursor, true
	case labelConductor:
		return ProviderConductor, true
	case labelZed, labelZedAgent, labelZedAgentUnderscore:
		return ProviderZed, true
	default:
		if strings.HasPrefix(string(label), passthroughOverridePrefix) {
			return ProviderOpenAICompat, true
		}
		return ProviderUnspecified, false
	}
}

// MustParse returns the parsed provider or ProviderUnspecified.
func MustParse(raw string) Provider {
	provider, ok := Parse(raw)
	if !ok {
		return ProviderUnspecified
	}
	return provider
}

// String returns the stable text form used in JSON cache files.
func (p Provider) String() string {
	switch p {
	case ProviderUnspecified:
		return "unspecified"
	case ProviderClaude:
		return "claude"
	case ProviderCodex:
		return "codex"
	case ProviderAnthropic:
		return "anthropic"
	case ProviderOpenAICompat:
		return "openai_compat"
	case ProviderMITM:
		return "mitm"
	case ProviderArtifact:
		return "artifact"
	case ProviderCursor:
		return "cursor"
	case ProviderConductor:
		return "conductor"
	case ProviderZed:
		return "zed"
	default:
		return "unspecified"
	}
}

// MarshalText emits the stable provider label for text encoders.
func (p Provider) MarshalText() ([]byte, error) {
	return []byte(p.String()), nil
}

// MarshalJSON emits the stable provider label for JSON cache files.
func (p Provider) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(p.String())), nil
}

// Valid reports whether p is one of Clyde's concrete provider enum values.
func (p Provider) Valid() bool {
	switch p {
	case ProviderUnspecified:
		return false
	case ProviderClaude, ProviderCodex, ProviderAnthropic, ProviderOpenAICompat, ProviderMITM, ProviderArtifact, ProviderCursor, ProviderConductor, ProviderZed:
		return true
	default:
		return false
	}
}
