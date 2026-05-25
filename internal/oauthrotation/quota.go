package oauthrotation

// quotaStatus is the in-memory observation of an account's last rate-limit
// state, derived from the upstream anthropic-ratelimit-unified-status header.
// It is intentionally typed (rather than a bare string) so callers cannot pass
// an arbitrary value into the selection code path.
type quotaStatus string

const (
	// quotaUnknown is the zero value. A slot starts in this state after a
	// daemon restart, and selection treats it as eligible.
	quotaUnknown quotaStatus = ""
	// quotaAllowed mirrors anthropic-ratelimit-unified-status: allowed. The
	// slot is fully eligible.
	quotaAllowed quotaStatus = "allowed"
	// quotaAllowedWarning mirrors the allowed_warning value: the request was
	// served but Anthropic is signalling an approaching limit. The slot is
	// still eligible.
	quotaAllowedWarning quotaStatus = "allowed_warning"
	// quotaRejected mirrors the rejected value: Anthropic refused the request
	// for this account. Combined with a freshness check, selection skips the
	// slot until the reset time or the freshness window expires.
	quotaRejected quotaStatus = "rejected"
)

// String returns the wire value so logs and tests can compare against the
// upstream header text directly.
func (q quotaStatus) String() string { return string(q) }
