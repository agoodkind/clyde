// Package oauth manages adapter OAuth token flows and persistence.
package oauth

import (
	"context"
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

func readCredentialCandidates(ctx context.Context, options oauthcredentials.ReadOptions) []oauthcredentials.ReadResult {
	options.Now = oauthClock.Now()
	return oauthcredentials.ReadCandidates(ctx, options)
}

func selectCredentialCandidate(results []oauthcredentials.ReadResult) (*selectedCredential, error) {
	summaries := oauthcredentials.Summarize(results)
	var selected *oauthcredentials.ReadResult
	for i := range results {
		candidate := &results[i]
		if !candidateUsable(candidate) {
			continue
		}
		if selected == nil || credentialCandidateBetter(candidate, selected) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, &OAuthCredentialError{
			Message:   "no usable claudeAiOauth credentials found; run `claude /login`",
			Summaries: summaries,
		}
	}
	return &selectedCredential{
		Source:    selected.Source,
		Tokens:    selected.Tokens.Clone(),
		Metadata:  selected.Metadata,
		Summaries: summaries,
	}, nil
}

func selectRefreshableCredential(results []oauthcredentials.ReadResult) (*selectedCredential, error) {
	summaries := oauthcredentials.Summarize(results)
	var selected *oauthcredentials.ReadResult
	for i := range results {
		candidate := &results[i]
		if !candidateRefreshable(candidate) {
			continue
		}
		if selected == nil || credentialCandidateBetter(candidate, selected) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, &OAuthCredentialError{
			Message:   "no refreshable claudeAiOauth credentials found; run `claude /login`",
			Summaries: summaries,
		}
	}
	return &selectedCredential{
		Source:    selected.Source,
		Tokens:    selected.Tokens.Clone(),
		Metadata:  selected.Metadata,
		Summaries: summaries,
	}, nil
}

func writeCredentials(ctx context.Context, options oauthcredentials.ReadOptions, source oauthcredentials.Source, tokens *Tokens) error {
	result := oauthcredentials.Write(ctx, options, source, tokens)
	if result.Err != nil {
		return result.Err
	}
	oauthLog.Logger().InfoContext(ctx, "oauth.credentials.refreshed_store_written",
		"subcomponent", "oauth",
		"store_kind", source,
		"expires_at_ms", tokens.ExpiresAt,
		"fingerprint", oauthcredentials.Fingerprint(tokens),
		"access_token_present", tokens.AccessToken != "",
		"refresh_token_present", tokens.RefreshToken != "",
	)
	return nil
}

func snapshotForCredential(selected *selectedCredential) credentialSnapshot {
	if selected == nil {
		return emptyCredentialSnapshot()
	}
	return credentialSnapshot{
		Source:              selected.Source,
		Fingerprint:         selected.Metadata.Fingerprint,
		ExpiresAt:           selected.Metadata.ExpiresAt,
		RefreshTokenPresent: selected.Metadata.RefreshTokenPresent,
		FileMtime:           selected.Metadata.FileMtime,
	}
}

func summariesAsStrings(summaries []oauthcredentials.Summary) []string {
	values := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		values = append(values, fmt.Sprintf("%s:present=%t:access=%t:refresh=%t:expired=%t:fingerprint=%s:error=%s",
			summary.Source,
			summary.Present,
			summary.AccessTokenPresent,
			summary.RefreshTokenPresent,
			summary.Expired,
			summary.Fingerprint,
			summary.ParseError,
		))
	}
	return values
}

func candidateUsable(candidate *oauthcredentials.ReadResult) bool {
	return candidate != nil && candidate.Err == nil && candidate.Tokens != nil && candidate.Metadata.AccessTokenPresent
}

func candidateRefreshable(candidate *oauthcredentials.ReadResult) bool {
	return candidateUsable(candidate) && candidate.Metadata.RefreshTokenPresent
}

func credentialCandidateBetter(candidate, selected *oauthcredentials.ReadResult) bool {
	if candidate.Metadata.Expired != selected.Metadata.Expired {
		return !candidate.Metadata.Expired
	}
	if candidate.Metadata.RefreshTokenPresent != selected.Metadata.RefreshTokenPresent {
		return candidate.Metadata.RefreshTokenPresent
	}
	if candidate.Source != selected.Source {
		return candidate.Source == oauthcredentials.SourceKeychain
	}
	return candidate.Metadata.ExpiresAt > selected.Metadata.ExpiresAt
}

func coalesce(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func splitScopes(raw string, fallback []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	return strings.Fields(raw)
}
