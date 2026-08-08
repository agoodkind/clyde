package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/gklog/trace"
)

const (
	cacheFilename   = "conversation-index.json"
	refreshDebounce = 30 * time.Second
	// cacheFormatVersion is the record shape the current binary writes. A cache
	// written by an older shape still loads its records, so startup stays fast,
	// but its file stamps are dropped so the first refresh re-parses every
	// artifact once and fills in the fields the old shape never stored. Raise it
	// whenever a scan starts deriving a record field the old cache cannot supply.
	//
	// Version 2 covers fields added together, because the version marks the record
	// shape rather than counting fields, and one raise re-parses the corpus once for
	// all of them.
	//
	// A Cursor chat's workspace root, origin, archived flag, and fallback title come
	// from a store version 1 never opened, and a chat's own artifact does not change
	// when any of those change, so without the raise the overwhelming majority would
	// keep a version 1 record with an empty workspace forever.
	//
	// Record.LatestRequestID is derived the same way: without the raise, an artifact
	// whose stamp is unchanged keeps its version 1 record and never gains the field,
	// so every conversation not written to since the upgrade resolves no request id.
	//
	// Version 3 covers the subagents/ twin rule: a top-level Cursor transcript
	// whose uuid appears under a sibling conversation's subagents/ directory now
	// derives subagent origin from that twin. A dispatched conversation's twin
	// file is finished and never changes again, so without the raise every twin
	// cached by version 2 keeps its user-origin record forever and stays in the
	// index and the semantic feed.
	cacheFormatVersion = 4
)

// Index owns the derived raw conversation cache. It resolves each artifact's
// parser through the registry it is constructed with, so the daemon wires the
// provider parsers in once and every read and scan dispatches through them.
type Index struct {
	mu sync.Mutex
	// includeSubagents exposes conversations a dispatched agent wrote. It is read
	// when the index serves records rather than when it scans, so the cache holds
	// every conversation regardless of the setting and flipping the setting needs
	// no cache deletion and no re-parse.
	includeSubagents bool
	registry         *Registry
	records          []Record
	prevRecords      map[string]Record
	prevStamps       map[string]FileStamp
	loaded           bool
	refreshing       bool
	// refreshRun is the refresh currently in flight and the outcome it finished
	// with. A synchronous Refresh waits on it rather than returning, because every
	// read path starts a background refresh a moment before it asks for a
	// synchronous one, so returning early would hand the caller back the same stale
	// snapshot it already failed against.
	refreshRun   *refreshRun
	lastRefresh  time.Time
	cachePath    string
	debounce     time.Duration
	scanProvider func(context.Context, *Registry, scanCache) (scanResult, error)
}

// NewIndex returns an index backed by the default XDG cache path that resolves
// artifacts through the provided registry and serves records under the given
// conversation settings. Callers are expected to register the provider parsers
// they need before constructing the index.
func NewIndex(registry *Registry, conversationConfig config.ConversationConfig) *Index {
	return &Index{
		mu:               sync.Mutex{},
		includeSubagents: conversationConfig.IncludeSubagentConversations,
		registry:         registry,
		records:          nil,
		prevRecords:      nil,
		prevStamps:       nil,
		loaded:           false,
		refreshing:       false,
		refreshRun:       nil,
		lastRefresh:      time.Time{},
		cachePath:        filepath.Join(config.GlobalCacheDir(), cacheFilename),
		debounce:         refreshDebounce,
		scanProvider:     scan,
	}
}

// Start runs a periodic debounced cache refresh until ctx is canceled.
func (idx *Index) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	// Load the cached records and their file stamps before the first refresh so
	// the worker reuses unchanged transcripts and re-parses only what changed,
	// rather than re-reading the whole corpus on every start.
	if err := idx.loadOnce(); err != nil {
		slog.WarnContext(ctx, "conversation.index.start_load_failed", "concern", "conversation.index", "component", "conversation", "err", err)
	}
	idx.refreshAsync(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			idx.refreshAsync(ctx)
		}
	}
}

// List returns the cached records and starts a debounced background refresh.
func (idx *Index) List(ctx context.Context) ([]Record, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}
	idx.refreshAsync(ctx)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.visibleRecords(), nil
}

// visibleRecords returns the cached records this index serves under its subagent
// setting. Every read path funnels through it, so a conversation a dispatched
// agent wrote is absent from listing, paging, search scoping, and the semantic
// sync feeder alike while the setting hides it. Callers hold idx.mu.
func (idx *Index) visibleRecords() []Record {
	if idx.includeSubagents {
		return cloneRecords(idx.records)
	}
	out := make([]Record, 0, len(idx.records))
	for _, record := range idx.records {
		if record.IsSubagent() {
			continue
		}
		out = append(out, record)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ListWithStamps returns the cached records with their artifact file stamps and
// starts a debounced background refresh.
func (idx *Index) ListWithStamps(ctx context.Context) ([]StampedRecord, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}
	idx.refreshAsync(ctx)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return cloneStampedRecords(idx.visibleRecords(), idx.prevStamps), nil
}

// RecordByID returns the cached record with the exact id from the in-memory
// snapshot, taking no refresh. The second result is false when no record
// matches.
func (idx *Index) RecordByID(id string) (Record, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return emptyRecord(), false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	// A hidden record is skipped rather than returned as a miss, so a subagent
	// conversation that somehow shares an id with a real one can never mask it.
	for _, record := range idx.records {
		if record.ID != id {
			continue
		}
		if record.IsSubagent() && !idx.includeSubagents {
			continue
		}
		return record, true
	}
	return emptyRecord(), false
}

func (idx *Index) recordsSnapshot() []Record {
	idx.mu.Lock()
	records := idx.visibleRecords()
	idx.mu.Unlock()
	return records
}

// Resolve finds one conversation by id, native id, title, artifact path, or
// provider request id.
//
// A selector shaped like a request id is matched against the identifiers a
// record owns and then taken to [Index.resolveUUIDShapedSelector]. It never
// reaches a title, exactly or by substring. A request id names one exact thing,
// while a title is a summary of what a conversation is about, so a conversation
// whose title happens to be those 36 characters is not the conversation that
// issued the request, and pasting a request id into a chat is how a title comes
// to be one.
func (idx *Index) Resolve(ctx context.Context, selector string) (Record, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Record{}, errors.New("conversation selector is required")
	}
	records, err := idx.List(ctx)
	if err != nil {
		return Record{}, err
	}
	if looksLikeRequestID(selector) {
		if record, ok := resolveRecordIdentity(records, selector); ok {
			return record, nil
		}
		return idx.resolveUUIDShapedSelector(ctx, selector)
	}
	if record, ok := resolveRecordExact(records, selector); ok {
		return record, nil
	}
	if record, ok := resolveRecordFuzzyTitle(records, selector); ok {
		return record, nil
	}
	fresh := idx.refreshBeforeLookup(ctx)
	records = idx.recordsSnapshot()
	if record, ok := resolveRecord(records, selector); ok {
		return record, nil
	}
	if !fresh {
		return Record{}, fmt.Errorf("conversation %q not found: %s", selector, staleIndexNote)
	}
	return Record{}, fmt.Errorf("conversation %q not found", selector)
}

// staleIndexNote is what a lookup says when it could not bring the index up to
// date before answering, so a miss over records that may be out of date is not
// presented as a confirmed absence.
const staleIndexNote = "the conversation index could not be refreshed, so this miss may be stale"

// refreshBeforeLookup brings the records up to date when it can, and reports
// whether it managed to.
//
// A lookup wants fresh records and it wants an answer. Failing the whole command
// because the refresh timed out or its scan errored trades the second for the
// first, and does so exactly where the caller is under most pressure: the first
// lookup after a cache-format upgrade re-parses the whole corpus under a
// background rescan that runs uncancellably, so a caller under an RPC deadline
// would get a deadline error where the records it already had would have
// answered. The refresh is therefore best effort, it is logged when it fails, and
// the caller reports the miss as possibly stale rather than as a determination.
func (idx *Index) refreshBeforeLookup(ctx context.Context) bool {
	if err := idx.Refresh(ctx); err != nil {
		slog.WarnContext(ctx, "conversation.index.lookup_refresh_failed", "concern", "conversation.index", "component", "conversation", "err", err)
		return false
	}
	return true
}

// resolveUUIDShapedSelector finishes resolution for a selector shaped like a
// UUID, which every provider request id is written in and so are Claude session
// ids, Codex thread ids, and Cursor composer ids.
//
// The shape check cannot tell those apart, so the cheap index paths run first
// and in full. A refresh and a second exact pass come before the provider is
// asked anything, which is what a native id absent from a stale cache relies on
// and what keeps an id the index already knows off the provider's live store
// entirely.
//
// Only after that does the provider's bounded request-id lookup run. The
// exhaustive provider scan is never reached from here: it is opt-in and lives on
// [Index.ResolveRequest].
//
// Nothing here fails the caller's command over a provider's store. This runs for
// every UUID-shaped selector of every provider, so a Cursor directory Clyde
// cannot read would otherwise turn an ordinary Claude conversation miss into an
// unrelated permission error. A store that could not be read makes the miss
// inconclusive instead, which is what it is.
// The index's own request-id pass runs here rather than in the exact matcher, so
// every selector surface answers a request id the same way `resolve-request`
// does, ambiguity included.
func (idx *Index) resolveUUIDShapedSelector(ctx context.Context, selector string) (Record, error) {
	fresh := idx.refreshBeforeLookup(ctx)
	records := idx.recordsSnapshot()
	if record, ok := resolveRecordIdentity(records, selector); ok {
		return record, nil
	}
	switch record, carriers := recordByLatestRequestID(records, selector); {
	case carriers == 1:
		return record, nil
	case carriers > 1:
		return Record{}, fmt.Errorf("conversation %q not found: %s", selector,
			notFoundNote(RequestNotFoundReasonAmbiguousConversation, fresh))
	}

	match := idx.resolveRequestMatch(ctx, selector, RequestLookupOptions{AllowFullScan: false})
	if !match.Found {
		return Record{}, fmt.Errorf("conversation %q not found: %s", selector, notFoundNote(match.Reason, fresh))
	}
	if record, ok := recordByNativeID(records, match.Provider, match.NativeConversationID); ok {
		return record, nil
	}
	return Record{}, fmt.Errorf(
		"conversation %q not found: %s",
		selector,
		notFoundNote(RequestNotFoundReasonUnindexedConversation, fresh),
	)
}

// notFoundNote renders the reason a lookup missed, and says so was over records
// that may be out of date when the refresh before it did not succeed.
func notFoundNote(reason RequestNotFoundReason, fresh bool) string {
	if fresh {
		return reason.Describe()
	}
	return reason.Describe() + "; " + staleIndexNote
}

// ResolveRequest maps one provider request id to the conversation that issued
// it, and reports which path answered.
//
// It tries Clyde's own index first, which is derived from artifact headers the
// scan already reads and so costs nothing extra. When the index does not know
// the id it asks each registered provider resolver for a bounded lookup against
// the provider's live store. The exhaustive provider scan runs only when the
// caller sets opts.AllowFullScan, because it costs tens of seconds.
//
// The index pass runs after the refresh rather than before it. Answering from
// the cached records first would be answering a question about how many
// conversations carry the id over a snapshot that predates the duplicate: a chat
// the operator copied since the last scan carries the request as truly as the
// original does, and the cache still shows one carrier, so the cheap path would
// name a conversation the current corpus calls ambiguous. The refresh is also
// what a cold cache relies on to know the id at all, so running it first costs a
// rebuild the fall-through was going to pay for anyway.
//
// An id no path resolves reports not found with the reason, never a nearby
// conversation.
func (idx *Index) ResolveRequest(
	ctx context.Context,
	requestID string,
	opts RequestLookupOptions,
) (RequestResolution, error) {
	requestID = normalizeRequestID(requestID)
	if requestID == "" {
		return RequestResolution{}, errors.New("request id is required")
	}
	// List is what loads the cache, which the refresh then scans incrementally
	// against. Its records are deliberately not consulted here.
	if _, err := idx.List(ctx); err != nil {
		return RequestResolution{}, err
	}
	// A refresh that does not finish is not fatal; it makes a miss inconclusive,
	// because the records the miss was established over may be the stale ones.
	fresh := idx.refreshBeforeLookup(ctx)
	records := idx.recordsSnapshot()
	if record, carriers := recordByLatestRequestID(records, requestID); carriers == 1 {
		return foundRequestResolution(requestID, RequestOriginIndex, record), nil
	} else if carriers > 1 {
		return ambiguousRequestResolution(ctx, requestID, carriers), nil
	}
	return idx.resolveRequestLive(ctx, requestID, opts, records, fresh), nil
}

// ambiguousRequestResolution reports a request id several conversations carry.
// Answering with one of them would be answering with whichever the record order
// happened to reach, which is the guess this operation exists not to make.
func ambiguousRequestResolution(ctx context.Context, requestID string, carriers int) RequestResolution {
	slog.InfoContext(ctx, "conversation.index.request_carried_by_several_conversations", "concern", "conversation.index", "component", "conversation", "request_id", requestID, "count", carriers)
	return missingRequestResolution(requestID, RequestNotFoundReasonAmbiguousConversation)
}

// resolveRequestLive asks the providers, then maps the native conversation id
// they report back onto an index record.
func (idx *Index) resolveRequestLive(
	ctx context.Context,
	requestID string,
	opts RequestLookupOptions,
	records []Record,
	fresh bool,
) RequestResolution {
	match := idx.resolveRequestMatch(ctx, requestID, opts)
	if !match.Found {
		reason := match.Reason
		if !fresh {
			reason = MergeRequestNotFoundReason(reason, RequestNotFoundReasonInconclusive)
		}
		return missingRequestResolution(requestID, reason)
	}
	record, ok := recordByNativeID(records, match.Provider, match.NativeConversationID)
	if !ok {
		// "The index does not hold it" is a claim about the index, and it can only
		// be made over an index that is current. A refresh that did not finish is
		// the likelier reason the conversation is missing than the provider having
		// invented it.
		reason := RequestNotFoundReasonUnindexedConversation
		if !fresh {
			reason = MergeRequestNotFoundReason(reason, RequestNotFoundReasonInconclusive)
		}
		return missingRequestResolution(requestID, reason)
	}
	return foundRequestResolution(requestID, match.Origin, record)
}

// resolveRequestMatch asks each registered provider resolver, in a stable
// provider order, which conversation issued a request id. It reads no index
// records, so a caller can run it before deciding whether a refresh is worth it.
//
// A miss carries the reason [MergeRequestNotFoundReason] settles on, so an
// inconclusive result outranks a confirmed one: if any provider could not read
// part of its store, the corpus-level answer cannot be a confirmed absence.
//
// A resolver that returns an error is one of those providers. Its store is what
// broke, and the id may well be sitting in it, so the error is logged and the
// provider contributes an inconclusive miss instead of failing the lookup. One
// provider's unreadable store must not decide the corpus answer, and it must not
// become the error a caller asking about another provider's conversation sees.
func (idx *Index) resolveRequestMatch(
	ctx context.Context,
	requestID string,
	opts RequestLookupOptions,
) RequestMatch {
	resolvers := idx.requestResolvers()
	if len(resolvers) == 0 {
		return missingRequestMatch(RequestNotFoundReasonNoResolver)
	}

	reason := RequestNotFoundReasonUnspecified
	for _, resolver := range resolvers {
		match, err := resolver.ResolveRequestID(ctx, requestID, opts)
		if err != nil {
			slog.WarnContext(ctx, "conversation.index.request_resolver_failed", "concern", "conversation.index", "component", "conversation", "request_id", requestID, "err", err)
			reason = MergeRequestNotFoundReason(reason, RequestNotFoundReasonInconclusive)
			continue
		}
		if match.Found {
			return match
		}
		reason = MergeRequestNotFoundReason(reason, match.Reason)
	}
	return missingRequestMatch(reason)
}

func missingRequestMatch(reason RequestNotFoundReason) RequestMatch {
	return RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               RequestOriginUnspecified,
		Reason:               reason,
	}
}

// requestResolvers returns the registered parsers that resolve request ids, in
// ascending provider order so the answer does not depend on map iteration.
func (idx *Index) requestResolvers() []RequestResolver {
	providers := idx.registry.Providers()
	slices.Sort(providers)
	resolvers := make([]RequestResolver, 0, len(providers))
	for _, provider := range providers {
		parser, err := idx.registry.Lookup(provider)
		if err != nil {
			continue
		}
		resolver, ok := parser.(RequestResolver)
		if !ok {
			continue
		}
		resolvers = append(resolvers, resolver)
	}
	return resolvers
}

func foundRequestResolution(requestID string, origin RequestOrigin, record Record) RequestResolution {
	return RequestResolution{
		RequestID: requestID,
		Found:     true,
		Origin:    origin,
		Reason:    RequestNotFoundReasonUnspecified,
		Record:    record,
	}
}

func missingRequestResolution(requestID string, reason RequestNotFoundReason) RequestResolution {
	return RequestResolution{
		RequestID: requestID,
		Found:     false,
		Origin:    RequestOriginUnspecified,
		Reason:    reason,
		Record:    emptyRecord(),
	}
}

// recordByLatestRequestID finds the conversation whose most recent turn carries
// the request id, and reports how many conversations carry it. The comparison is
// whole-string, so a record that merely contains the id in some other field never
// matches.
//
// The count is what the caller needs, not just the first hit. Duplicating a
// conversation copies the field: measured on a real Cursor store, 43 of 1,836
// chats advertising a latest request id share it with another chat, so the first
// record carrying one is not evidence that it is the only one.
func recordByLatestRequestID(records []Record, requestID string) (Record, int) {
	if requestID == "" {
		return emptyRecord(), 0
	}
	first := emptyRecord()
	carriers := 0
	for _, record := range records {
		if record.LatestRequestID != requestID {
			continue
		}
		if carriers == 0 {
			first = record
		}
		carriers++
	}
	return first, carriers
}

// recordByNativeID maps a provider's own conversation id back to the index
// record for it.
//
// It is deliberately narrower than [resolveRecordExact], which is a
// human-selector matcher and also compares titles and artifact paths across every
// provider. A native id arriving here is not something a human typed: a resolver
// stated it, and the only record that can answer for it is the one that provider
// owns under that id. Going through the selector matcher instead lets a
// conversation from any provider whose title happens to be that id answer, so a
// Cursor composer id pasted into a Claude chat while debugging returns the Claude
// conversation as the one that issued the request.
func recordByNativeID(records []Record, provider providerid.Provider, nativeID string) (Record, bool) {
	if nativeID == "" {
		return emptyRecord(), false
	}
	for _, record := range records {
		if record.Provider == provider && record.NativeID == nativeID {
			return record, true
		}
	}
	return emptyRecord(), false
}

// Refresh rebuilds the cache synchronously, and waits for the rebuild already in
// flight when one is running rather than returning while it is still going.
//
// Waiting on someone else's rebuild reports that rebuild's outcome, so a nil
// error means the records were rebuilt, whoever rebuilt them.
func (idx *Index) Refresh(ctx context.Context) (err error) {
	defer trace.Op(ctx, "conversation.index.refresh")(&err)
	run, prior, claimed := idx.beginRefresh()
	if !claimed {
		return idx.waitForRefresh(ctx, run)
	}
	defer func() { idx.endRefresh(run, err) }()

	result, err := idx.scanProvider(ctx, idx.registry, prior)
	if err != nil {
		return err
	}
	sortRecords(result.records)
	if err = writeCache(idx.cachePath, result.records, result.stamps); err != nil {
		return err
	}
	idx.installRefreshResult(result)
	idx.reportSkippedSubagents(ctx, result.records)
	return nil
}

// refreshRun is one rebuild in flight and what it produced. The outcome travels
// with the channel because a caller that waits on someone else's rebuild is
// asking whether the records are now rebuilt, and a closed channel alone answers
// only that the rebuild stopped.
type refreshRun struct {
	done chan struct{}
	// err is the rebuild's outcome, written once before done is closed and read
	// only after it is, which is what orders the two without a second lock.
	err error
}

// beginRefresh claims the single refresh slot. The third result is false when
// another refresh already holds it, and the returned run is then the one already
// in flight.
func (idx *Index) beginRefresh() (*refreshRun, scanCache, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.refreshing {
		return idx.refreshRun, scanCache{records: nil, stamps: nil}, false
	}
	idx.refreshing = true
	idx.refreshRun = &refreshRun{done: make(chan struct{}), err: nil}
	return idx.refreshRun, scanCache{records: idx.prevRecords, stamps: idx.prevStamps}, true
}

// endRefresh records the rebuild's outcome, releases the refresh slot, and wakes
// every caller waiting on it. It runs after the records are installed, so a woken
// caller reads the new snapshot rather than the one it was waiting to replace.
func (idx *Index) endRefresh(run *refreshRun, err error) {
	run.err = err
	idx.mu.Lock()
	idx.refreshing = false
	if idx.refreshRun == run {
		idx.refreshRun = nil
	}
	idx.mu.Unlock()
	close(run.done)
}

// waitForRefresh blocks until the refresh already in flight finishes, and
// reports what it produced.
//
// A rebuild that returned an error installed nothing, so reporting the wait as a
// success would tell the caller the records are rebuilt when they are the same
// ones it was waiting to replace. The background rebuild logs its own failure,
// but the waiter is a different caller and the log is not an answer to it.
func (idx *Index) waitForRefresh(ctx context.Context, run *refreshRun) error {
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		if run.err != nil {
			slog.WarnContext(ctx, "conversation.index.awaited_refresh_failed", "concern", "conversation.index", "component", "conversation", "err", run.err)
			return fmt.Errorf("await conversation index refresh: %w", run.err)
		}
		return nil
	case <-ctx.Done():
		slog.WarnContext(ctx, "conversation.index.refresh_wait_canceled", "concern", "conversation.index", "component", "conversation", "err", ctx.Err())
		return fmt.Errorf("wait for conversation index refresh: %w", ctx.Err())
	}
}

func (idx *Index) installRefreshResult(result scanResult) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.records = result.records
	idx.prevRecords = recordsByPath(result.records)
	idx.prevStamps = result.stamps
	idx.loaded = true
	idx.lastRefresh = clock.Now()
}

// reportSkippedSubagents names how many conversations the setting is hiding, so
// someone hunting for a conversation that never appeared can see why. It says
// nothing when the setting exposes subagent conversations or when the scan found
// none.
func (idx *Index) reportSkippedSubagents(ctx context.Context, records []Record) {
	idx.mu.Lock()
	includeSubagents := idx.includeSubagents
	idx.mu.Unlock()
	if includeSubagents {
		return
	}
	skipped := 0
	for _, record := range records {
		if record.IsSubagent() {
			skipped++
		}
	}
	if skipped == 0 {
		return
	}
	slog.InfoContext(ctx, "conversation.index.subagent_conversations_skipped",
		"concern", "conversation.index", "component", "conversation",
		"count", skipped, "setting", "conversation.include_subagent_conversations")
}

func (idx *Index) loadOnce() error {
	idx.mu.Lock()
	if idx.loaded {
		idx.mu.Unlock()
		return nil
	}
	idx.mu.Unlock()

	records, stamps, err := readCache(idx.cachePath)
	if err != nil {
		return err
	}
	idx.mu.Lock()
	idx.records = records
	idx.prevRecords = recordsByPath(records)
	idx.prevStamps = stamps
	idx.loaded = true
	idx.mu.Unlock()
	return nil
}

func (idx *Index) refreshAsync(ctx context.Context) {
	idx.mu.Lock()
	debounced := clock.Now().Sub(idx.lastRefresh) < idx.debounce
	idx.mu.Unlock()
	if debounced {
		return
	}
	run, prior, claimed := idx.beginRefresh()
	if !claimed {
		return
	}
	go func() {
		// The outcome is recorded on the run so a caller of Refresh that waits on
		// this rebuild is told what it produced, rather than only that it stopped.
		var err error
		defer func() { idx.endRefresh(run, err) }()
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("conversation index refresh panicked: %v", recovered)
				slog.ErrorContext(ctx, "conversation.index.refresh_panic", "concern", "conversation.index", "component", "conversation", "err", err)
			}
		}()
		result, err := idx.scanProvider(context.WithoutCancel(ctx), idx.registry, prior)
		if err == nil {
			sortRecords(result.records)
			err = writeCache(idx.cachePath, result.records, result.stamps)
		}
		if err != nil {
			slog.WarnContext(ctx, "conversation.index.background_refresh_failed", "concern", "conversation.index", "component", "conversation", "err", err)
			return
		}
		idx.installRefreshResult(result)
		idx.reportSkippedSubagents(ctx, result.records)
	}()
}

// recordsByPath indexes records by artifact path so the next incremental scan
// can reuse the record for any file whose stamp is unchanged.
func recordsByPath(records []Record) map[string]Record {
	byPath := make(map[string]Record, len(records))
	for _, record := range records {
		byPath[recordKey(record.ArtifactPath, record.Selector)] = record
	}
	return byPath
}

// cacheFile is the on-disk index cache. It persists the per-file stamps next to
// the records so a freshly started worker reuses them on its first refresh and
// re-parses only changed transcripts, instead of re-reading the whole corpus on
// every start.
type cacheFile struct {
	// Version is the record shape the writing binary used. A file written before
	// the field existed decodes to zero, which is older than every real version.
	Version int                  `json:"version"`
	Records []Record             `json:"records"`
	Stamps  map[string]FileStamp `json:"stamps"`
}

func readCache(path string) ([]Record, map[string]FileStamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		slog.Warn("conversation.index.cache_read_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, nil, fmt.Errorf("read conversation cache: %w", err)
	}
	var cache cacheFile
	if err := json.Unmarshal(data, &cache); err == nil && cache.Records != nil {
		sortRecords(cache.Records)
		if cache.Version < cacheFormatVersion {
			// The records still serve reads immediately, but their stamps are
			// dropped so the first refresh re-parses every artifact once and
			// derives the fields this shape added. Without that, an unchanged file
			// would keep its old record forever and never gain an origin.
			slog.Info("conversation.index.cache_format_upgraded", "concern", "conversation.index", "component", "conversation", "path", path, "from_version", cache.Version, "to_version", cacheFormatVersion, "records", len(cache.Records))
			return cache.Records, nil, nil
		}
		return cache.Records, cache.Stamps, nil
	}
	// Fall back to the legacy records-only array. The next write upgrades the
	// file to the stamped envelope, so the first refresh re-parses once and then
	// the startup path is cheap.
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		slog.Warn("conversation.index.cache_decode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, nil, fmt.Errorf("decode conversation cache: %w", err)
	}
	sortRecords(records)
	return records, nil, nil
}

func writeCache(path string, records []Record, stamps map[string]FileStamp) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("conversation.index.cache_mkdir_failed", "concern", "conversation.index", "component", "conversation", "path", filepath.Dir(path), "err", err)
		return fmt.Errorf("create conversation cache dir: %w", err)
	}
	data, err := json.MarshalIndent(cacheFile{Version: cacheFormatVersion, Records: records, Stamps: stamps}, "", "  ")
	if err != nil {
		slog.Warn("conversation.index.cache_encode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return fmt.Errorf("encode conversation cache: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		slog.Warn("conversation.index.cache_write_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return fmt.Errorf("write conversation cache: %w", err)
	}
	slog.Debug("conversation.index.cache_written", "concern", "conversation.index", "component", "conversation", "path", path, "records", len(records))
	return nil
}

func resolveRecord(records []Record, selector string) (Record, bool) {
	if record, ok := resolveRecordExact(records, selector); ok {
		return record, true
	}
	return resolveRecordFuzzyTitle(records, selector)
}

// resolveRecordExact matches a selector against the identifiers a record owns
// outright. Every comparison is whole-string, so a record that merely contains
// the selector somewhere never matches here.
//
// A request id is deliberately not one of them. It does not identify one
// conversation: duplicating a chat copies the field, and 43 of 1,836 chats
// advertising one share it with another chat, so returning the first record
// carrying it answers with whichever sorted first. Matching it here would also
// answer before [Index.Resolve] reaches the request-id route, so every selector
// surface would quietly disagree with `resolve-request` about the same id.
// [recordByLatestRequestID] is the one place that lookup happens, and it counts.
func resolveRecordExact(records []Record, selector string) (Record, bool) {
	if record, ok := resolveRecordIdentity(records, selector); ok {
		return record, true
	}
	for _, record := range records {
		if record.Title == selector {
			return record, true
		}
	}
	return emptyRecord(), false
}

// resolveRecordIdentity is [resolveRecordExact] without the title, for a
// selector that names one exact thing.
//
// A title is what a conversation is about rather than what it is, so it can be
// any string at all, request ids included: pasting one into a chat is enough to
// make it that chat's title. Matching a request-id-shaped selector against a
// title would hand back a conversation that merely discussed the request as the
// conversation that issued it, and would do so before the request-id route ran,
// so every selector surface would quietly disagree with `resolve-request` about
// the same id.
func resolveRecordIdentity(records []Record, selector string) (Record, bool) {
	for _, record := range records {
		if record.ID == selector ||
			record.NativeID == selector ||
			record.ArtifactPath == selector {
			return record, true
		}
	}
	return emptyRecord(), false
}

// resolveRecordFuzzyTitle matches a selector against record titles by substring,
// and answers only when exactly one record matches. It is a convenience for
// human-typed titles and it can return a conversation the caller did not name,
// so it must never run for a selector that identifies one exact thing.
func resolveRecordFuzzyTitle(records []Record, selector string) (Record, bool) {
	lower := strings.ToLower(selector)
	var matched Record
	matchCount := 0
	for _, record := range records {
		if strings.Contains(strings.ToLower(record.Title), lower) {
			matched = record
			matchCount++
		}
	}
	if matchCount != 1 {
		return emptyRecord(), false
	}
	return matched, true
}

func sortRecords(records []Record) {
	sort.SliceStable(records, func(i int, j int) bool {
		left := records[i]
		right := records[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
}

func cloneRecords(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}
	out := make([]Record, len(records))
	copy(out, records)
	return out
}

func cloneStampedRecords(records []Record, stamps map[string]FileStamp) []StampedRecord {
	if len(records) == 0 {
		return nil
	}
	out := make([]StampedRecord, 0, len(records))
	for _, record := range records {
		out = append(out, StampedRecord{
			Record: record,
			Stamp:  stamps[recordKey(record.ArtifactPath, record.Selector)],
		})
	}
	return out
}

// DerivedID returns the stable conversation id for an artifact: the provider
// label joined to the native provider id, or an artifact-hash id when the
// native id is missing. The provider parser packages call it so every record
// shares one id derivation.
func DerivedID(provider Provider, providerID string, artifactPath string) string {
	providerID = strings.TrimSpace(providerID)
	if providerID != "" {
		return provider.String() + ":" + providerID
	}
	sum := sha256.Sum256([]byte(artifactPath))
	return ProviderArtifact.String() + ":" + hex.EncodeToString(sum[:8])
}
