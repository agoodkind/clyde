package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/search"
	searchstore "goodkind.io/clyde/internal/search/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	// defaultMaxConcurrentJobs bounds simultaneous search jobs.
	defaultMaxConcurrentJobs = 2
	// defaultAnalyzeModel is the fallback model for the analyze pass when no
	// search model is configured.
	defaultAnalyzeModel = "qwen2.5-coder-32b"
	// defaultWithinSearchLimit bounds a within-conversation search when the
	// caller does not set one.
	defaultWithinSearchLimit = 20
	// withinWindowBefore and withinWindowAfter size the message window attached
	// to each hit, so a match carries its surrounding exchange.
	withinWindowBefore = 2
	withinWindowAfter  = 2
)

// hit sources tag where a within-search match came from: the engine's stored
// vectors or the literal transcript scan that covers unindexed content.
const (
	hitSourceSemantic = "semantic"
	hitSourceLiteral  = "literal"
)

// WithinSearchOptions narrows a within-conversation search by row attributes.
// The zero value matches everything with the default limit.
type WithinSearchOptions struct {
	Limit     int
	Roles     []string
	FromUnix  int64
	UntilUnix int64
	MinScore  float64
}

// WithinSearchClient is the engine-backed retrieval surface a within search
// uses for candidates: scoped hits plus the fingerprint the engine has
// embedded for the conversation. The semsearch client satisfies it.
type WithinSearchClient interface {
	SearchWithinConversation(ctx context.Context, collectionID, conversationID, query string, limit int32, filter semsearch.SearchFilter) ([]semsearch.SemHit, string, error)
}

// withinHit is one selected match: the message it points at, where it came
// from, and the engine score when semantic.
type withinHit struct {
	MessageIndex int
	Source       string
	Score        float64
}

// storedExcerpt is one matched message persisted for a completed search so a
// later analyze call can rebuild the prompt without re-running the search.
// MessageIndex, Source, and Score carry the retrieval provenance.
type storedExcerpt struct {
	Role         string    `json:"role"`
	Timestamp    time.Time `json:"timestamp"`
	Text         string    `json:"text"`
	MessageIndex int       `json:"message_index,omitempty"`
	Source       string    `json:"source,omitempty"`
	Score        float64   `json:"score,omitempty"`
}

// storedResultSet is the JSON shape persisted in the search job store's
// result-set column. It is a flattened, self-contained projection of the search
// matches so the store stays free of conversation and transcript types.
type storedResultSet struct {
	ConversationID string          `json:"conversation_id"`
	Excerpts       []storedExcerpt `json:"excerpts"`
}

// SearchJobMeta is the per-job metadata stored alongside each active livetrack
// handle the search job manager registers.
type SearchJobMeta struct {
	ResultID       string
	ConversationID string
}

// IsLivetrackMeta satisfies the livetrack.Meta constraint.
func (SearchJobMeta) IsLivetrackMeta() {}

// searchJobCloser cancels a job's context when livetrack drains the registry.
type searchJobCloser struct {
	cancel context.CancelFunc
}

// Close cancels the job context. It satisfies livetrack.Closer.
func (c *searchJobCloser) Close(_ string) error {
	c.cancel()
	return nil
}

// SearchJobManager runs conversation searches as background jobs, persisting
// their state and results to a SQLite store so a later poll, analyze, or cancel
// can reach them and so job state survives a daemon reload. Every running job
// is registered with internal/livetrack so reload and shutdown drain it.
type SearchJobManager struct {
	index *Index
	store *searchstore.Store
	cfg   config.SearchConfig
	log   *slog.Logger
	// semantic is the engine retrieval client, nil when conversation semantic
	// search is not configured; semanticCollectionID names its collection.
	semantic             WithinSearchClient
	semanticCollectionID string
	registry             *livetrack.Registry[SearchJobMeta]
	sem                  chan struct{}

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewSearchJobManager constructs a manager. Background jobs run under contexts
// derived with [context.WithoutCancel] from the request that started them, so a
// job outlives the RPC that kicked it off.
func NewSearchJobManager(index *Index, store *searchstore.Store, cfg config.SearchConfig, log *slog.Logger, semantic WithinSearchClient, semanticCollectionID string, group *livetrack.Group) *SearchJobManager {
	if log == nil {
		log = slog.Default()
	}
	if group == nil {
		group = livetrack.NewGroup(livetrack.GroupOptions{Log: log})
	}
	// Search jobs attach as a non-quiet-relevant PhaseWorkers member: background
	// jobs are internal restartable work, so they must not hold a reload to its
	// quiet max-wait, but they still drain in the workers phase on reload.
	registry := livetrack.Attach[SearchJobMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseWorkers,
		QuietRelevant: false,
		CancelNoWait:  false,
	}, livetrack.Options[SearchJobMeta]{
		Component:     "conversation",
		Concern:       "conversation.search",
		Log:           log,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
	return &SearchJobManager{
		index:                index,
		store:                store,
		cfg:                  cfg,
		log:                  log,
		semantic:             semantic,
		semanticCollectionID: strings.TrimSpace(semanticCollectionID),
		registry:             registry,
		sem:                  make(chan struct{}, defaultMaxConcurrentJobs),
		mu:                   sync.Mutex{},
		cancels:              make(map[string]context.CancelFunc),
	}
}

// Start resolves a conversation, records a pending job, spawns the search in a
// background goroutine, and returns the result id immediately. The caller polls
// Status for the final result; retrieval completes in about a second.
func (m *SearchJobManager) Start(ctx context.Context, conversationID, query string, opts WithinSearchOptions) (string, error) {
	record, err := m.index.Resolve(ctx, conversationID)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.resolve_failed", "concern", "conversation.search", "conversation_id", conversationID, "err", err)
		return "", fmt.Errorf("resolve conversation: %w", err)
	}
	resultUUID, err := uuid.NewRandom()
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.result_id_failed", "concern", "conversation.search", "err", err)
		return "", fmt.Errorf("generate result id: %w", err)
	}
	resultID := resultUUID.String()
	bgCtx := context.WithoutCancel(ctx)
	job := searchstore.Job{
		ResultID:       resultID,
		ConversationID: record.ID,
		Query:          query,
		Depth:          "",
		Status:         searchstore.StatusPending,
		CreatedAt:      time.Time{},
		UpdatedAt:      time.Time{},
		Progress:       searchstore.Progress{ChunksDone: 0, ChunksTotal: 0, LayerIndex: 0, LayerTotal: 0, LayerName: ""},
		ResultText:     "",
		ResultSetJSON:  nil,
		Error:          "",
	}
	if err := m.store.Insert(bgCtx, job); err != nil {
		m.log.WarnContext(ctx, "conversation.search.insert_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return "", fmt.Errorf("insert search job: %w", err)
	}
	m.spawn(bgCtx, record, resultID, query, opts)
	return resultID, nil
}

// Status returns the stored job for a result id. The bool is false when no such
// job exists.
func (m *SearchJobManager) Status(ctx context.Context, resultID string) (searchstore.Job, bool, error) {
	job, ok, err := m.store.Get(ctx, resultID)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.status_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		var zero searchstore.Job
		return zero, false, fmt.Errorf("get search status: %w", err)
	}
	return job, ok, nil
}

// Cancel cancels a running or pending job and marks it canceled. Canceling an
// already-terminal job is a no-op.
func (m *SearchJobManager) Cancel(ctx context.Context, resultID string) error {
	job, ok, err := m.store.Get(ctx, resultID)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.cancel_lookup_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return fmt.Errorf("look up search job: %w", err)
	}
	if !ok {
		return fmt.Errorf("result_id %q not found", resultID)
	}
	switch job.Status {
	case searchstore.StatusComplete, searchstore.StatusFailed, searchstore.StatusCanceled:
		return nil
	case searchstore.StatusPending, searchstore.StatusRunning:
		// Proceed to cancellation below.
	}
	m.mu.Lock()
	cancel := m.cancels[resultID]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := m.store.Cancel(ctx, resultID); err != nil {
		m.log.WarnContext(ctx, "conversation.search.cancel_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return fmt.Errorf("mark search canceled: %w", err)
	}
	return nil
}

// Analyze runs the configured analysis model over a completed job's stored
// result set. It refuses cleanly when the job is missing or not yet complete.
func (m *SearchJobManager) Analyze(ctx context.Context, resultID, prompt string) (string, error) {
	if _, err := uuid.Parse(resultID); err != nil {
		return "", fmt.Errorf("invalid result_id %q", resultID)
	}
	job, ok, err := m.store.Get(ctx, resultID)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.analyze_lookup_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return "", fmt.Errorf("look up search job: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("result_id %q not found", resultID)
	}
	if job.Status != searchstore.StatusComplete {
		return "", fmt.Errorf("search %q is not complete yet (status %s)", resultID, job.Status)
	}
	if len(job.ResultSetJSON) == 0 {
		return "", fmt.Errorf("search %q has no results to analyze", resultID)
	}
	var stored storedResultSet
	if err := json.Unmarshal(job.ResultSetJSON, &stored); err != nil {
		m.log.WarnContext(ctx, "conversation.search.analyze_decode_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return "", fmt.Errorf("decode result set: %w", err)
	}
	if len(stored.Excerpts) == 0 {
		return "", fmt.Errorf("stored result has no messages")
	}
	return m.analyzeExcerpts(ctx, stored, prompt)
}

func (m *SearchJobManager) spawn(bgCtx context.Context, record Record, resultID, query string, opts WithinSearchOptions) {
	jobCtx, cancel := context.WithCancel(bgCtx)
	meta := SearchJobMeta{ResultID: resultID, ConversationID: record.ID}
	handle, regErr := m.registry.Register(jobCtx, "search.job", meta, &searchJobCloser{cancel: cancel})
	if regErr != nil {
		cancel()
		m.failJob(bgCtx, resultID, "server draining, search rejected")
		return
	}
	m.track(resultID, cancel)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.log.ErrorContext(jobCtx, "conversation.search.job_panic", "concern", "conversation.search", "result_id", resultID, "err", fmt.Errorf("panic: %v", r))
				m.failJob(bgCtx, resultID, fmt.Sprintf("panic: %v", r))
			}
			m.registry.Release(jobCtx, handle, "search.job.done")
			m.untrack(resultID)
			cancel()
		}()
		m.runJob(jobCtx, bgCtx, resultID, record, query, opts)
	}()
}

func (m *SearchJobManager) runJob(searchCtx, storeCtx context.Context, resultID string, record Record, query string, opts WithinSearchOptions) {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-searchCtx.Done():
		m.cancelJob(storeCtx, resultID)
		return
	}

	if err := m.store.Start(storeCtx, resultID); err != nil {
		m.log.WarnContext(storeCtx, "conversation.search.start_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultWithinSearchLimit
	}

	messages, err := m.index.LoadMessages(record, false, false)
	if err != nil {
		m.failJob(storeCtx, resultID, fmt.Sprintf("load conversation: %v", err))
		return
	}
	if len(messages) == 0 {
		m.failJob(storeCtx, resultID, "conversation has no messages")
		return
	}

	semanticHits, indexedFingerprint := m.engineWithinHits(searchCtx, record, query, limit, opts)
	if searchCtx.Err() != nil {
		m.cancelJob(storeCtx, resultID)
		return
	}
	// The literal scan covers what the engine has not embedded: a missing or
	// trailing fingerprint means the transcript holds content the index does
	// not, so matches there are found by scanning the messages directly.
	literalNeeded := m.semantic == nil || indexedFingerprint == "" || indexedFingerprint != artifactFingerprint(record)
	hits := mergeWithinHits(semanticHits, messages, query, opts, limit, literalNeeded)
	if len(hits) == 0 {
		m.completeJob(storeCtx, resultID, "No matching messages found.", nil)
		return
	}

	stored := buildWithinResultSet(record.ID, messages, hits)
	raw, err := json.Marshal(stored)
	if err != nil {
		m.failJob(storeCtx, resultID, fmt.Sprintf("encode result set: %v", err))
		return
	}
	text := renderWithinResults(resultID, record, messages, hits, literalNeeded)
	m.completeJob(storeCtx, resultID, text, raw)
}

// engineWithinHits asks the engine for scoped candidates. A nil client or a
// failed call returns no hits and an empty fingerprint, which routes the job
// to the literal scan.
func (m *SearchJobManager) engineWithinHits(ctx context.Context, record Record, query string, limit int, opts WithinSearchOptions) ([]semsearch.SemHit, string) {
	if m.semantic == nil || m.semanticCollectionID == "" {
		return nil, ""
	}
	filter := semsearch.SearchFilter{
		Roles:                opts.Roles,
		FromUnix:             opts.FromUnix,
		UntilUnix:            opts.UntilUnix,
		ConversationIDs:      nil,
		ParentConversationID: "",
		MinScore:             opts.MinScore,
		MessageIndexFrom:     0,
		MessageIndexUntil:    0,
	}
	hits, indexedFingerprint, err := m.semantic.SearchWithinConversation(ctx, m.semanticCollectionID, record.ID, query, int32Limit(limit), filter)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.engine_failed", "concern", "conversation.search", "conversation_id", record.ID, "err", err)
		return nil, ""
	}
	return hits, indexedFingerprint
}

// artifactFingerprint stamps the conversation's transcript file the same way
// the semantic feeder does, so comparing it to the engine's checkpointed
// fingerprint says whether the index covers the current transcript.
func artifactFingerprint(record Record) string {
	info, err := os.Stat(record.ArtifactPath)
	if err != nil {
		return ""
	}
	return FileStamp{Size: info.Size(), Mtime: info.ModTime()}.Fingerprint()
}

// mergeWithinHits builds the final hit list: engine hits first in relevance
// order, then, when the index does not cover the transcript, literal matches
// in transcript order, deduplicated by message index and bounded by limit.
func mergeWithinHits(semanticHits []semsearch.SemHit, messages []transcript.Message, query string, opts WithinSearchOptions, limit int, literalNeeded bool) []withinHit {
	hits := make([]withinHit, 0, limit)
	taken := make(map[int]bool, limit)
	for _, hit := range semanticHits {
		index := int(hit.MessageIndex)
		if index < 0 || index >= len(messages) || taken[index] {
			continue
		}
		taken[index] = true
		hits = append(hits, withinHit{MessageIndex: index, Source: hitSourceSemantic, Score: hit.Score})
		if len(hits) >= limit {
			return hits
		}
	}
	if !literalNeeded {
		return hits
	}
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return hits
	}
	for index, message := range messages {
		if len(hits) >= limit {
			break
		}
		if taken[index] || !messageMatchesOptions(message, opts) || !messageMatchesTerms(message, terms) {
			continue
		}
		taken[index] = true
		hits = append(hits, withinHit{MessageIndex: index, Source: hitSourceLiteral, Score: 0})
	}
	return hits
}

// messageMatchesOptions applies the role and time conditions to one message in
// the literal path, mirroring the engine-side filter semantics: inclusive
// from, exclusive until, case-insensitive roles.
func messageMatchesOptions(message transcript.Message, opts WithinSearchOptions) bool {
	return messageMatchesRowFilters(message, opts.Roles, opts.FromUnix, opts.UntilUnix)
}

// int32Limit clamps a search limit into int32 range for the wire.
func int32Limit(limit int) int32 {
	if limit > math.MaxInt32 {
		return math.MaxInt32
	}
	if limit < 0 {
		return 0
	}
	return int32(limit)
}

// buildWithinResultSet projects the hits into the stored result set: one
// excerpt per hit carrying the hit message's role, time, and windowed text, so
// a later analyze sees each match with its surrounding exchange.
func buildWithinResultSet(conversationID string, messages []transcript.Message, hits []withinHit) storedResultSet {
	excerpts := make([]storedExcerpt, 0, len(hits))
	for _, hit := range hits {
		hitMessage := messages[hit.MessageIndex]
		excerpts = append(excerpts, storedExcerpt{
			Role:         hitMessage.Role,
			Timestamp:    hitMessage.Timestamp,
			Text:         windowText(messages, hit.MessageIndex),
			MessageIndex: hit.MessageIndex,
			Source:       hit.Source,
			Score:        hit.Score,
		})
	}
	return storedResultSet{ConversationID: conversationID, Excerpts: excerpts}
}

// windowText renders the messages around one hit (withinWindowBefore before,
// withinWindowAfter after) as plain text with index, time, and role markers.
func windowText(messages []transcript.Message, hitIndex int) string {
	start := max(hitIndex-withinWindowBefore, 0)
	end := min(hitIndex+withinWindowAfter+1, len(messages))
	var out strings.Builder
	for index := start; index < end; index++ {
		message := messages[index]
		marker := " "
		if index == hitIndex {
			marker = ">"
		}
		fmt.Fprintf(&out, "%s[#%d][%s] %s:\n", marker, index, message.Timestamp.Format("2006-01-02 15:04"), displayRole(message.Role))
		if message.Text != "" {
			out.WriteString(message.Text)
			out.WriteString("\n")
		}
	}
	return out.String()
}

// transcriptRole models the provider role strings this package renders.
type transcriptRole string

const (
	transcriptRoleUser      transcriptRole = "user"
	transcriptRoleAssistant transcriptRole = "assistant"
)

// displayRole capitalizes the two common roles and passes anything else
// through unchanged.
func displayRole(role string) string {
	switch transcriptRole(role) {
	case transcriptRoleAssistant:
		return "Assistant"
	case transcriptRoleUser:
		return "User"
	default:
		return role
	}
}

func (m *SearchJobManager) completeJob(ctx context.Context, resultID, text string, raw json.RawMessage) {
	if err := m.store.Complete(ctx, resultID, text, raw); err != nil {
		m.log.WarnContext(ctx, "conversation.search.complete_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
	}
}

func (m *SearchJobManager) failJob(ctx context.Context, resultID, message string) {
	if err := m.store.Fail(ctx, resultID, message); err != nil {
		m.log.WarnContext(ctx, "conversation.search.fail_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
	}
}

func (m *SearchJobManager) cancelJob(ctx context.Context, resultID string) {
	if err := m.store.Cancel(ctx, resultID); err != nil {
		m.log.WarnContext(ctx, "conversation.search.cancel_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
	}
}

func (m *SearchJobManager) analyzeExcerpts(ctx context.Context, stored storedResultSet, prompt string) (string, error) {
	excerpts := buildAnalyzeExcerpts(stored.Excerpts)
	fullPrompt := fmt.Sprintf("%s\n\nCONVERSATION EXCERPTS from conversation %q:\n\n%s", prompt, stored.ConversationID, excerpts)
	model := m.cfg.Local.Model
	if model == "" {
		model = defaultAnalyzeModel
	}
	client := search.NewClientForModel(m.cfg, model)
	response, err := client.Complete(ctx, fullPrompt)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.analysis_failed", "concern", "conversation.search", "err", err)
		return "", fmt.Errorf("complete analysis prompt: %w", err)
	}
	return response, nil
}

func buildAnalyzeExcerpts(excerpts []storedExcerpt) string {
	var out strings.Builder
	for _, excerpt := range excerpts {
		fmt.Fprintf(&out, "[%s] %s:\n%s\n\n", excerpt.Timestamp.Format("2006-01-02 15:04"), displayRole(excerpt.Role), excerpt.Text)
	}
	return out.String()
}

func (m *SearchJobManager) track(resultID string, cancel context.CancelFunc) {
	m.mu.Lock()
	m.cancels[resultID] = cancel
	m.mu.Unlock()
}

func (m *SearchJobManager) untrack(resultID string) {
	m.mu.Lock()
	delete(m.cancels, resultID)
	m.mu.Unlock()
}
