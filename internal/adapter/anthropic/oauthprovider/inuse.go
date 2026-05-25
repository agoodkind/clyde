package oauthprovider

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/inuse"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// inUseCacheTTL bounds how long one detection result is reused. The detector
// re-runs the keychain read and process scan when the cache is older than
// this. Plan-pinned at 30 seconds.
const inUseCacheTTL = 30 * time.Second

// claudeProcessName is the basename the detector matches when filtering
// process rows returned by `ps -A -o pid,comm`.
const claudeProcessName = "claude"

// psBinary is the binary the detector shells out to. macOS ships it as `ps`;
// Linux procps offers the same flags under the same name.
const psBinary = "ps"

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

// DaemonPIDSource returns the PIDs of Claude worker processes the daemon
// spawned and currently tracks through livetrack. The detector excludes these
// PIDs from the external-session count so a daemon-owned `claude` worker is
// not mistaken for an external Claude Code session.
type DaemonPIDSource func() []int

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

// processScanner returns the list of PIDs for running `claude` binaries on
// the host. The default implementation shells out to `ps`; tests replace it
// with a deterministic in-memory scanner to avoid spawning real processes.
type processScanner func(ctx context.Context) ([]int, error)

// InUseDetector implements inuse.Detector for the Anthropic provider. It
// reads the Claude Code keychain entry, computes its fingerprint, enumerates
// external `claude` processes (excluding daemon-spawned PIDs), and reports
// the rotator account whose slot fingerprint matches the keychain. Results
// are cached for inUseCacheTTL, keyed by the keychain fingerprint.
type InUseDetector struct {
	keychain    KeychainReader
	fingerprint FingerprintFunc
	daemonPIDs  DaemonPIDSource
	accounts    FingerprintSource
	scanProcs   processScanner
	now         func() time.Time
	logger      *slog.Logger

	mu        sync.Mutex
	cached    inuse.Set
	cachedKey string
	cachedAt  time.Time
}

// InUseDeps carries the dependencies NewInUseDetector needs. KeychainReader
// and Fingerprint are required for the detector to do useful work; nil
// FingerprintSource and DaemonPIDSource default to functions returning empty
// slices, so a partially wired daemon still produces a safe empty set. Now
// defaults to [time.Now] and Logger defaults to [slog.Default] when nil.
type InUseDeps struct {
	KeychainReader   KeychainReader
	Fingerprint      FingerprintFunc
	DaemonPIDs       DaemonPIDSource
	AccountSnapshots FingerprintSource
	Now              func() time.Time
	Logger           *slog.Logger
}

// NewInUseDetector builds an InUseDetector from the supplied dependencies.
// A nil keychain reader yields a detector that always returns the empty set,
// which keeps the rotator wiring valid before a keychain service is
// configured. A nil fingerprint helper is also tolerated and produces the
// same behavior.
func NewInUseDetector(deps InUseDeps) *InUseDetector {
	det := newInUseDetectorWithScanner(deps, nil)
	det.scanProcs = det.defaultProcessScanner()
	return det
}

// newInUseDetectorWithScanner is the test-friendly constructor: it builds the
// detector without installing the default process scanner so tests can inject
// a deterministic one. Production callers go through NewInUseDetector which
// wires the `ps` shell-out.
func newInUseDetectorWithScanner(deps InUseDeps, scanner processScanner) *InUseDetector {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	accounts := deps.AccountSnapshots
	if accounts == nil {
		accounts = func() []AccountFingerprint { return nil }
	}
	daemonPIDs := deps.DaemonPIDs
	if daemonPIDs == nil {
		daemonPIDs = func() []int { return nil }
	}
	return &InUseDetector{
		keychain:    deps.KeychainReader,
		fingerprint: deps.Fingerprint,
		daemonPIDs:  daemonPIDs,
		accounts:    accounts,
		scanProcs:   scanner,
		now:         now,
		logger:      logger,
		mu:          sync.Mutex{},
		cached:      inuse.NewSet(),
		cachedKey:   "",
		cachedAt:    time.Time{},
	}
}

// Detect satisfies inuse.Detector. A keychain or process-enumeration failure
// returns an empty set and logs a warn event so the rotator treats it as
// "nothing in use" rather than blocking refreshes. The accounts argument is
// accepted for the contract but not used: the detector reads the live
// fingerprint snapshot from its FingerprintSource so a freshly imported
// account is matchable without restarting the daemon.
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

	d.mu.Lock()
	if d.cachedKey == keychainFingerprint && d.now().Sub(d.cachedAt) < inUseCacheTTL {
		cached := d.cached
		d.mu.Unlock()
		return cached, nil
	}
	d.mu.Unlock()

	externalPIDs, scanErr := d.externalClaudePIDs(ctx)
	if scanErr != nil {
		d.logger.WarnContext(ctx, "anthropic.inuse.process_scan_failed",
			"subcomponent", "anthropic",
			"err", scanErr.Error(),
		)
		return inuse.NewSet(), nil
	}

	if len(externalPIDs) == 0 {
		empty := inuse.NewSet()
		d.storeCached(keychainFingerprint, empty)
		return empty, nil
	}

	matched := d.matchAccount(keychainFingerprint)
	d.storeCached(keychainFingerprint, matched)
	return matched, nil
}

// storeCached records the latest detection under the lock so the next call
// inside inUseCacheTTL skips the keychain and process scans.
func (d *InUseDetector) storeCached(key string, set inuse.Set) {
	d.mu.Lock()
	d.cached = set
	d.cachedKey = key
	d.cachedAt = d.now()
	d.mu.Unlock()
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

// externalClaudePIDs runs the configured process scanner and removes any
// PID the daemon spawned, returning the list of external `claude` PIDs.
// An exec or parse failure surfaces from the underlying scanner so the
// caller can log and fall back to the empty set.
func (d *InUseDetector) externalClaudePIDs(ctx context.Context) ([]int, error) {
	allPIDs, err := d.scanProcs(ctx)
	if err != nil {
		return nil, err
	}
	if len(allPIDs) == 0 {
		return nil, nil
	}
	excluded := make(map[int]struct{}, 0)
	for _, pid := range d.daemonPIDs() {
		excluded[pid] = struct{}{}
	}
	external := make([]int, 0, len(allPIDs))
	for _, pid := range allPIDs {
		if _, drop := excluded[pid]; drop {
			continue
		}
		external = append(external, pid)
	}
	return external, nil
}

// defaultProcessScanner shells out to `ps -A -o pid,comm` and parses claude
// PIDs from the output. It is the production scanner installed by
// NewInUseDetector; tests substitute an in-memory scanner. A debug event
// satisfies the staticcheck-extra boundary-logging rule for the external
// process invocation; the warn event in Detect covers the user-facing
// failure path.
func (d *InUseDetector) defaultProcessScanner() processScanner {
	return func(ctx context.Context) ([]int, error) {
		d.logger.DebugContext(ctx, "anthropic.inuse.process_scan",
			"subcomponent", "anthropic",
			"bin", psBinary,
		)
		cmd := exec.CommandContext(ctx, psBinary, "-A", "-o", "pid,comm")
		out, err := cmd.Output()
		if err != nil {
			d.logger.WarnContext(ctx, "anthropic.inuse.process_scan_exec_failed",
				"subcomponent", "anthropic",
				"bin", psBinary,
				"err", err.Error(),
			)
			return nil, fmt.Errorf("exec %s: %w", psBinary, err)
		}
		return parseClaudePIDs(string(out)), nil
	}
}

// parseClaudePIDs scans `ps -A -o pid,comm` output for rows whose comm
// basename is `claude` and returns their PIDs. The header row is skipped
// because its second column is "COMM", not "claude". The parser is lenient
// against extra whitespace and against absolute paths in the comm column on
// platforms that emit them.
func parseClaudePIDs(psOutput string) []int {
	out := make([]int, 0)
	for line := range strings.SplitSeq(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		comm := fields[len(fields)-1]
		if filepath.Base(comm) != claudeProcessName {
			continue
		}
		out = append(out, pid)
	}
	return out
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
			SecurityBinary:  "",
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
