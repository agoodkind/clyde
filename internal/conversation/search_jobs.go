package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/search"
	searchstore "goodkind.io/clyde/internal/search/store"
	"goodkind.io/clyde/internal/util"
)

const (
	// defaultMaxConcurrentJobs bounds simultaneous search jobs. The local
	// backend serializes on one loaded model, so a small limit avoids thrashing
	// model loads while still letting a second request queue without blocking.
	defaultMaxConcurrentJobs = 2
	// defaultAnalyzeModel is the fallback model for the analyze pass when no
	// search model is configured.
	defaultAnalyzeModel = "qwen2.5-coder-32b"
)

// storedExcerpt is one matched message persisted for a completed search so a
// later analyze call can rebuild the prompt without re-running the search.
type storedExcerpt struct {
	Role      string    `json:"role"`
	Timestamp time.Time `json:"timestamp"`
	Text      string    `json:"text"`
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
	Depth          string
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
	index    *Index
	store    *searchstore.Store
	cfg      config.SearchConfig
	log      *slog.Logger
	registry *livetrack.Registry[SearchJobMeta]
	sem      chan struct{}

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewSearchJobManager constructs a manager. Background jobs run under contexts
// derived with [context.WithoutCancel] from the request that started them, so a
// job outlives the RPC that kicked it off.
func NewSearchJobManager(index *Index, store *searchstore.Store, cfg config.SearchConfig, log *slog.Logger) *SearchJobManager {
	if log == nil {
		log = slog.Default()
	}
	registry := livetrack.New[SearchJobMeta](livetrack.Options[SearchJobMeta]{
		Component:     "conversation",
		Concern:       "conversation.search",
		Log:           log,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
	return &SearchJobManager{
		index:    index,
		store:    store,
		cfg:      cfg,
		log:      log,
		registry: registry,
		sem:      make(chan struct{}, defaultMaxConcurrentJobs),
		mu:       sync.Mutex{},
		cancels:  make(map[string]context.CancelFunc),
	}
}

// Start resolves a conversation, records a pending job, spawns the search in a
// background goroutine, and returns the result id immediately. The caller polls
// Status for progress and the final result.
func (m *SearchJobManager) Start(ctx context.Context, conversationID, query, depth string) (string, error) {
	record, err := m.index.Resolve(ctx, conversationID)
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.resolve_failed", "concern", "conversation.search", "conversation_id", conversationID, "err", err)
		return "", fmt.Errorf("resolve conversation: %w", err)
	}
	resultID, err := util.GenerateUUIDE()
	if err != nil {
		m.log.WarnContext(ctx, "conversation.search.result_id_failed", "concern", "conversation.search", "err", err)
		return "", fmt.Errorf("generate result id: %w", err)
	}
	bgCtx := context.WithoutCancel(ctx)
	job := searchstore.Job{
		ResultID:       resultID,
		ConversationID: record.ID,
		Query:          query,
		Depth:          depth,
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
	m.spawn(bgCtx, record, resultID, query, depth)
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

// Drain cancels every in-flight job and waits for the registry to quiesce. The
// daemon calls it on reload and shutdown.
func (m *SearchJobManager) Drain(ctx context.Context, reason string) livetrack.DrainResult {
	return m.registry.Drain(ctx, reason)
}

func (m *SearchJobManager) spawn(bgCtx context.Context, record Record, resultID, query, depth string) {
	jobCtx, cancel := context.WithCancel(bgCtx)
	meta := SearchJobMeta{ResultID: resultID, ConversationID: record.ID, Depth: depth}
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
		m.runJob(jobCtx, bgCtx, resultID, record, query, depth)
	}()
}

func (m *SearchJobManager) runJob(searchCtx, storeCtx context.Context, resultID string, record Record, query, depth string) {
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

	messages, err := m.index.LoadMessages(record, false, false)
	if err != nil {
		m.failJob(storeCtx, resultID, fmt.Sprintf("load conversation: %v", err))
		return
	}
	if len(messages) == 0 {
		m.failJob(storeCtx, resultID, "conversation has no messages")
		return
	}

	results, err := search.WithDepthProgress(searchCtx, messages, query, m.cfg, depth, m.throttledProgress(storeCtx, resultID))
	if err != nil {
		if searchCtx.Err() != nil {
			m.cancelJob(storeCtx, resultID)
			return
		}
		m.failJob(storeCtx, resultID, fmt.Sprintf("search messages: %v", err))
		return
	}
	if len(results) == 0 {
		m.completeJob(storeCtx, resultID, "No matching messages found.", nil)
		return
	}

	stored := buildStoredResultSet(record.ID, results)
	raw, err := json.Marshal(stored)
	if err != nil {
		m.failJob(storeCtx, resultID, fmt.Sprintf("encode result set: %v", err))
		return
	}
	text := renderSearchResults(resultID, record, messages, results)
	m.completeJob(storeCtx, resultID, text, raw)
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

// throttledProgress returns a goroutine-safe progress callback that writes to
// the store at most every progressMinInterval, plus immediately on every layer
// change, so per-chunk reporting on a long conversation does not storm SQLite.
func (m *SearchJobManager) throttledProgress(ctx context.Context, resultID string) search.ProgressFunc {
	var mu sync.Mutex
	var lastWriteNano int64
	lastLayer := -1
	const progressMinIntervalNano = int64(400_000_000) // 400ms
	return func(p search.Progress) {
		mu.Lock()
		now := clock.Now().UnixNano()
		changedLayer := p.LayerIndex != lastLayer
		if !changedLayer && now-lastWriteNano < progressMinIntervalNano {
			mu.Unlock()
			return
		}
		lastWriteNano = now
		lastLayer = p.LayerIndex
		mu.Unlock()
		if err := m.store.UpdateProgress(ctx, resultID, searchstore.Progress{
			ChunksDone:  p.ChunksDone,
			ChunksTotal: p.ChunksTotal,
			LayerIndex:  p.LayerIndex,
			LayerTotal:  p.LayerTotal,
			LayerName:   p.LayerName,
		}); err != nil {
			m.log.WarnContext(ctx, "conversation.search.progress_write_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		}
	}
}

func (m *SearchJobManager) analyzeExcerpts(ctx context.Context, stored storedResultSet, prompt string) (string, error) {
	excerpts := buildAnalyzeExcerpts(stored.Excerpts)
	fullPrompt := fmt.Sprintf("%s\n\nCONVERSATION EXCERPTS from conversation %q:\n\n%s", prompt, stored.ConversationID, excerpts)
	model := m.cfg.Local.Model
	if len(m.cfg.Local.Pipeline) > 0 {
		model = m.cfg.Local.Pipeline[0].Model
	}
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

func buildStoredResultSet(conversationID string, results []search.Result) storedResultSet {
	var excerpts []storedExcerpt
	for _, result := range results {
		for _, message := range result.Messages {
			excerpts = append(excerpts, storedExcerpt{
				Role:      message.Role,
				Timestamp: message.Timestamp,
				Text:      message.Text,
			})
		}
	}
	return storedResultSet{ConversationID: conversationID, Excerpts: excerpts}
}

func buildAnalyzeExcerpts(excerpts []storedExcerpt) string {
	var out strings.Builder
	for _, excerpt := range excerpts {
		role := "User"
		if excerpt.Role == "assistant" {
			role = "Assistant"
		}
		fmt.Fprintf(&out, "[%s] %s:\n%s\n\n", excerpt.Timestamp.Format("2006-01-02 15:04"), role, excerpt.Text)
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
