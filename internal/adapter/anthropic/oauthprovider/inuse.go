// In-use detection signal: the daemon-owned MITM tunnels registry. When at
// least one active MITM session targets api.anthropic.com, Claude Code
// traffic is flowing through Clyde and the keychain-matched account is
// in-use. Sessions register through internal/mitm/connect_tunnel.go and are
// released on connection close, so the registry reports exactly what is
// live at the moment of the query.
//
// Setup constraint: a directly-launched `claude` binary that bypasses
// Clyde's MITM proxy (no HTTPS_PROXY, no MITM CA trust) is invisible to
// this detector by design. Coverage requires Claude traffic to traverse
// the daemon-owned MITM proxy; operator setups that route around it lose
// the signal but otherwise function normally.

package oauthprovider

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/inuse"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// AccountFingerprint pairs one rotator account id with the fingerprint of the
// credential currently in that account's slot. The detector compares each
// fingerprint against the keychain fingerprint to decide which rotator
// account, if any, the keychain credential corresponds to.
type AccountFingerprint struct {
	Account     provider.AccountID
	Fingerprint string
}

// FingerprintSource returns the current rotator account fingerprints for the
// Anthropic provider. The detector reads this on every detection so a freshly
// imported account becomes matchable without a process restart.
type FingerprintSource func() []AccountFingerprint

// ClaudeMITMTracker reports the count of active MITM sessions whose
// destination is under *.anthropic.com. The detector uses this as the
// "claude is currently active" signal; layer separation keeps the provider
// package free of an internal/mitm import, so the daemon wiring code owns
// the implementation and passes it in at registration time.
type ClaudeMITMTracker interface {
	ClaudeAnthropicSessionCount() int
}

// KeychainReader returns the credential bytes stored under the Claude Code
// keychain entry, plus a Present flag that is false when the entry is absent.
// The detector calls this on every detection and discards everything but the
// fingerprint so no token material persists past the call.
type KeychainReader func(ctx context.Context) (KeychainCredential, error)

// KeychainCredential is the credential view the detector consumes from a
// KeychainReader. Tokens backs oauthcredentials.Fingerprint; Present is false
// when the keychain entry is missing or empty.
type KeychainCredential struct {
	Tokens  *oauthcredentials.Tokens
	Present bool
}

// FingerprintFunc computes a stable, non-secret fingerprint for a keychain
// credential. The detector and the rotator slots must use the same function
// so the fingerprints compare directly; oauthcredentials.Fingerprint is the
// production choice.
type FingerprintFunc func(tokens *oauthcredentials.Tokens) string

// InUseDetector implements inuse.Detector for the Anthropic provider. It
// reads the Claude Code keychain entry, computes its fingerprint, asks the
// MITM tracker whether any claude-to-anthropic session is currently active,
// and reports the rotator account whose slot fingerprint matches the
// keychain. Result is computed fresh on every Detect call; the MITM tracker
// query is an O(n) walk over a small in-process registry, so caching adds
// staleness with no measurable saving.
type InUseDetector struct {
	keychain    KeychainReader
	fingerprint FingerprintFunc
	tracker     ClaudeMITMTracker
	accounts    FingerprintSource
	logger      *slog.Logger
}

// InUseDeps carries the dependencies NewInUseDetector needs. KeychainReader
// and Fingerprint are required for the detector to do useful work; nil
// FingerprintSource and MITMTracker default to safe no-op values that
// produce an empty set, so a partially wired daemon still returns a defined
// result. Logger defaults to [slog.Default] when nil.
type InUseDeps struct {
	KeychainReader   KeychainReader
	Fingerprint      FingerprintFunc
	MITMTracker      ClaudeMITMTracker
	AccountSnapshots FingerprintSource
	Logger           *slog.Logger
}

// noopTracker is the safe default when no MITM tracker is wired. It reports
// zero active sessions so the detector returns an empty set.
type noopTracker struct{}

// ClaudeAnthropicSessionCount returns zero. The detector treats this as
// "no claude traffic active", which yields an empty in-use set.
func (noopTracker) ClaudeAnthropicSessionCount() int { return 0 }

// NewInUseDetector builds an InUseDetector from the supplied dependencies.
// A nil keychain reader yields a detector that always returns the empty set,
// which keeps the rotator wiring valid before a keychain service is
// configured. A nil fingerprint helper is also tolerated and produces the
// same behavior.
func NewInUseDetector(deps InUseDeps) *InUseDetector {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	accounts := deps.AccountSnapshots
	if accounts == nil {
		accounts = func() []AccountFingerprint { return nil }
	}
	tracker := deps.MITMTracker
	if tracker == nil {
		tracker = noopTracker{}
	}
	return &InUseDetector{
		keychain:    deps.KeychainReader,
		fingerprint: deps.Fingerprint,
		tracker:     tracker,
		accounts:    accounts,
		logger:      logger,
	}
}

// Detect satisfies inuse.Detector. A keychain failure returns an empty set
// and logs a warn event so the rotator treats it as "nothing in use" rather
// than blocking refreshes. The accounts argument is accepted for the contract
// but not used: the detector reads the live fingerprint snapshot from its
// FingerprintSource so a freshly imported account is matchable without
// restarting the daemon.
func (d *InUseDetector) Detect(ctx context.Context, _ []provider.AccountID) (inuse.Set, error) {
	if d == nil || d.keychain == nil || d.fingerprint == nil {
		return inuse.NewSet(), nil
	}

	cred, err := d.keychain(ctx)
	if err != nil {
		d.logger.WarnContext(ctx, "anthropic.inuse.keychain_read_failed",
			"subcomponent", "anthropic",
			"err", err.Error(),
		)
		return inuse.NewSet(), nil
	}
	if !cred.Present || cred.Tokens == nil {
		return inuse.NewSet(), nil
	}
	keychainFingerprint := d.fingerprint(cred.Tokens)
	if keychainFingerprint == "" {
		return inuse.NewSet(), nil
	}

	if d.tracker.ClaudeAnthropicSessionCount() == 0 {
		return inuse.NewSet(), nil
	}

	return d.matchAccount(keychainFingerprint), nil
}

// matchAccount walks the rotator's current fingerprint snapshot and returns a
// set containing the account whose stored fingerprint equals the keychain
// fingerprint. When nothing matches, the returned set is empty: the keychain
// holds a credential the rotator has not imported yet, and the rotator owns
// no slot to skip.
func (d *InUseDetector) matchAccount(keychainFingerprint string) inuse.Set {
	for _, account := range d.accounts() {
		if account.Fingerprint == keychainFingerprint {
			return inuse.NewSet(account.Account)
		}
	}
	return inuse.NewSet()
}

// defaultKeychainReader builds a KeychainReader backed by
// oauthcredentials.ReadCandidates. It is selected only on darwin: on every
// other platform the underlying credential helpers route the keychain source
// to a file-backed store, so the reader returns Present=false and the
// detector falls back to "nothing in use".
func defaultKeychainReader(service string, now func() time.Time) KeychainReader {
	return func(ctx context.Context) (KeychainCredential, error) {
		results := oauthcredentials.ReadCandidates(ctx, oauthcredentials.ReadOptions{
			CredentialsDir:  "",
			KeychainService: service,
			Platform:        runtime.GOOS,
			Now:             now(),
		})
		for _, result := range results {
			if result.Source != oauthcredentials.SourceKeychain {
				continue
			}
			if result.Err != nil {
				return KeychainCredential{Tokens: nil, Present: false}, result.Err
			}
			if !result.Present || result.Tokens == nil {
				return KeychainCredential{Tokens: nil, Present: false}, nil
			}
			return KeychainCredential{Tokens: result.Tokens, Present: true}, nil
		}
		return KeychainCredential{Tokens: nil, Present: false}, nil
	}
}
