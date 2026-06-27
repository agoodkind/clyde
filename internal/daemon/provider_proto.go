package daemon

import (
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/providerid"
)

func providerFromProto(provider clydev1.Provider) providerid.Provider {
	switch provider {
	case clydev1.Provider_PROVIDER_CLAUDE:
		return providerid.ProviderClaude
	case clydev1.Provider_PROVIDER_CODEX:
		return providerid.ProviderCodex
	case clydev1.Provider_PROVIDER_ANTHROPIC:
		return providerid.ProviderAnthropic
	case clydev1.Provider_PROVIDER_OPENAI_COMPAT:
		return providerid.ProviderOpenAICompat
	case clydev1.Provider_PROVIDER_MITM:
		return providerid.ProviderMITM
	case clydev1.Provider_PROVIDER_ARTIFACT:
		return providerid.ProviderArtifact
	case clydev1.Provider_PROVIDER_CURSOR:
		return providerid.ProviderCursor
	case clydev1.Provider_PROVIDER_CONDUCTOR:
		return providerid.ProviderConductor
	case clydev1.Provider_PROVIDER_UNSPECIFIED:
		return providerid.ProviderUnspecified
	default:
		return providerid.ProviderUnspecified
	}
}

func protoProvider(provider providerid.Provider) clydev1.Provider {
	switch provider {
	case providerid.ProviderClaude:
		return clydev1.Provider_PROVIDER_CLAUDE
	case providerid.ProviderCodex:
		return clydev1.Provider_PROVIDER_CODEX
	case providerid.ProviderAnthropic:
		return clydev1.Provider_PROVIDER_ANTHROPIC
	case providerid.ProviderOpenAICompat:
		return clydev1.Provider_PROVIDER_OPENAI_COMPAT
	case providerid.ProviderMITM:
		return clydev1.Provider_PROVIDER_MITM
	case providerid.ProviderArtifact:
		return clydev1.Provider_PROVIDER_ARTIFACT
	case providerid.ProviderCursor:
		return clydev1.Provider_PROVIDER_CURSOR
	case providerid.ProviderConductor:
		return clydev1.Provider_PROVIDER_CONDUCTOR
	case providerid.ProviderUnspecified:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	default:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	}
}
