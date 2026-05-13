package search

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/lmctl"
)

const (
	// defaultChunkChars is the default target size for each conversation chunk.
	defaultChunkChars = 50_000
	// defaultMaxConcurrent is the default number of parallel LLM requests.
	defaultMaxConcurrent = 4
)

// Result holds matching messages from a search.
type Result struct {
	Messages []transcript.Message
	Summary  string // LLM's description of what matched
}

// SearchWithDepth finds conversation messages with a configurable search depth.
func SearchWithDepth(ctx context.Context, messages []transcript.Message, query string, cfg config.SearchConfig, depth string) ([]Result, error) {
	return searchInternal(ctx, slog.Default(), messages, query, cfg, depth)
}

func searchInternal(ctx context.Context, log *slog.Logger, messages []transcript.Message, query string, cfg config.SearchConfig, depth string) ([]Result, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	// "quick" is embedding-only: no LLM involved.
	if depth == "quick" {
		log.InfoContext(ctx, "search starting (embedding only)",
			"query", query,
			"messages", len(messages),
			"depth", depth,
		)
		start := searchClock.Now()
		results, err := embeddingOnlySearch(ctx, log, messages, query, cfg)
		log.InfoContext(ctx, "search complete",
			"total_matches", len(results),
			"total_duration", time.Since(start).Round(time.Millisecond),
		)
		return results, err
	}

	// "extra-deep" runs the full pipeline and warns about runtime.
	if depth == "extra-deep" {
		log.WarnContext(ctx, "extra-deep search uses the full pipeline including large model verification; this may take 10+ minutes")
	}

	pipeline := cfg.Local.ResolvePipeline(depth)
	if len(pipeline) == 0 {
		return nil, fmt.Errorf("no search pipeline configured")
	}

	log.InfoContext(ctx, "search starting",
		"query", query,
		"messages", len(messages),
		"depth", depth,
		"layers", len(pipeline),
	)
	searchStart := searchClock.Now()

	// Track current model so we only swap when needed
	currentModel := ""

	// Layer 1: sweep with fast model
	sweepLayer := pipeline[0]
	if cfg.Backend == "local" {
		log.InfoContext(ctx, "loading sweep model", "model", sweepLayer.Model)
		if err := lmctl.EnsureLoaded(ctx, sweepLayer.Model,
			lmctl.WithContextLength(cfg.Local.ContextLength),
			lmctl.WithMaxMemoryGB(cfg.Local.MaxMemoryGB),
			lmctl.WithWarmup(cfg.Local.URL, cfg.Local.ResolvedToken()),
		); err != nil {
			return nil, fmt.Errorf("failed to load model %s: %w", sweepLayer.Model, err)
		}
		currentModel = sweepLayer.Model
	}
	sweepStart := searchClock.Now()
	sweepClient := newClientForModel(cfg, sweepLayer.Model)
	matched := sweepChunks(ctx, log, sweepClient, messages, query, cfg)
	log.InfoContext(ctx, "sweep complete",
		"model", sweepLayer.Model,
		"matches", len(matched),
		"duration", time.Since(sweepStart).Round(time.Millisecond),
	)

	// Layer 2+: rerank/deep passes with progressively smarter models
	for i := 1; i < len(pipeline); i++ {
		if len(matched) == 0 {
			log.DebugContext(ctx, "no matches, skipping remaining layers")
			break
		}
		layer := pipeline[i]

		// Skip rerank if only 1 result (nothing to filter), but still run deep
		if len(matched) <= 1 && layer.Name != "deep" {
			log.DebugContext(ctx, "skipping rerank layer (single result)", "layer", layer.Name)
			continue
		}

		// Swap model if this layer uses a different one
		if cfg.Backend == "local" && layer.Model != currentModel {
			log.DebugContext(ctx, "swapping model", "from", currentModel, "to", layer.Model, "layer", layer.Name)
			if err := lmctl.EnsureLoaded(ctx, layer.Model,
				lmctl.WithContextLength(cfg.Local.ContextLength),
				lmctl.WithMaxMemoryGB(cfg.Local.MaxMemoryGB),
				lmctl.WithWarmup(cfg.Local.URL, cfg.Local.ResolvedToken()),
			); err != nil {
				log.WarnContext(ctx, "model load failed, skipping layer", "model", layer.Model, "err", err)
				continue
			}
			currentModel = layer.Model
		}

		layerStart := searchClock.Now()
		layerClient := newClientForModel(cfg, layer.Model)

		beforeCount := len(matched)
		switch layer.Name {
		case "deep":
			matched = deepAnalysis(ctx, layerClient, matched, query)
		default:
			matched = rerankResults(ctx, layerClient, matched, query)
		}
		log.DebugContext(ctx, "layer complete",
			"layer", layer.Name,
			"model", layer.Model,
			"before", beforeCount,
			"after", len(matched),
			"duration", time.Since(layerStart).Round(time.Millisecond),
		)
	}

	log.InfoContext(ctx, "search complete",
		"total_matches", len(matched),
		"total_duration", time.Since(searchStart).Round(time.Millisecond),
	)
	return matched, nil
}

// embeddingOnlySearch ranks chunks by cosine similarity and returns the top
// matches without any LLM call. Results include a similarity score in the summary.
func embeddingOnlySearch(ctx context.Context, log *slog.Logger, messages []transcript.Message, query string, cfg config.SearchConfig) ([]Result, error) {
	chunkSize := cfg.Local.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultChunkChars
	}
	chunks := chunkMessages(messages, chunkSize)
	log.InfoContext(ctx, "quick search: chunked messages", "chunks", len(chunks), "messages", len(messages))

	if cfg.Backend != "local" {
		return nil, fmt.Errorf("quick depth requires local backend with embedding model")
	}

	if err := ensureEmbeddingModelReady(ctx, cfg.Local); err != nil {
		log.ErrorContext(ctx, "quick search: embedding model load failed", "err", err)
		return nil, fmt.Errorf("failed to load embedding model: %w", err)
	}

	filter := newEmbeddingFilter(cfg.Local)

	// Build chunk texts
	chunkTexts := make([]string, len(chunks))
	for i, chunk := range chunks {
		var text string
		for _, m := range chunk {
			text += m.Text + "\n"
			if len(text) > 2000 {
				text = text[:2000]
				break
			}
		}
		chunkTexts[i] = text
	}

	// Embed query
	queryEmbStart := searchClock.Now()
	queryEmb, err := filter.embed(ctx, []string{query})
	if err != nil {
		log.ErrorContext(ctx, "quick search: query embedding failed", "err", err)
		return nil, fmt.Errorf("embedding query failed: %w", err)
	}
	log.DebugContext(ctx, "quick search: query embedded", "duration", time.Since(queryEmbStart).Round(time.Millisecond))

	// Embed all chunks in one batch
	chunksEmbStart := searchClock.Now()
	chunkEmbs, err := filter.embed(ctx, chunkTexts)
	if err != nil {
		log.ErrorContext(ctx, "quick search: chunk embedding failed", "chunks", len(chunkTexts), "err", err)
		return nil, fmt.Errorf("embedding chunks failed: %w", err)
	}
	log.DebugContext(ctx, "quick search: chunks embedded", "chunks", len(chunkTexts), "duration", time.Since(chunksEmbStart).Round(time.Millisecond))

	if len(queryEmb) == 0 || len(chunkEmbs) != len(chunks) {
		return nil, fmt.Errorf("unexpected embedding response lengths")
	}

	// Score and collect above-threshold chunks
	type scored struct {
		chunk []transcript.Message
		score float64
	}
	queryVec := queryEmb[0]
	var hits []scored
	for i, chunkVec := range chunkEmbs {
		sim := cosineSimilarity(queryVec, chunkVec)
		if sim >= filter.threshold {
			hits = append(hits, scored{chunk: chunks[i], score: sim})
		}
	}

	// Sort by score descending
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].score > hits[i].score {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}

	log.InfoContext(ctx, "quick search: embedding scored",
		"total_chunks", len(chunks),
		"hits", len(hits),
		"threshold", filter.threshold,
		"query_embed_duration", time.Since(queryEmbStart).Round(time.Millisecond),
		"chunks_embed_duration", time.Since(chunksEmbStart).Round(time.Millisecond),
	)

	results := make([]Result, len(hits))
	for i, h := range hits {
		results[i] = Result{
			Messages: h.chunk,
			Summary:  fmt.Sprintf("Embedding similarity: %.2f", h.score),
		}
	}
	return results, nil
}

// sweepChunks runs the initial parallel chunk search.
func sweepChunks(ctx context.Context, log *slog.Logger, client Client, messages []transcript.Message, query string, cfg config.SearchConfig) []Result {
	chunkSize := cfg.Local.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultChunkChars
	}
	chunks := chunkMessages(messages, chunkSize)
	log.InfoContext(ctx, "sweep: chunked messages", "chunks", len(chunks), "messages", len(messages), "chunk_size", chunkSize)

	// Pre-filter with embeddings if available (local backend only).
	// The embedding model is tiny (~0.1 GB) and can coexist with inference models.
	if cfg.Backend == "local" {
		_ = ensureEmbeddingModelReady(ctx, cfg.Local)
		filter := newEmbeddingFilter(cfg.Local)
		chunks = filter.filterChunks(ctx, query, chunks)
	}

	type chunkResult struct {
		idx    int
		result *Result
		err    error
	}

	type searchConcurrencyPermit struct {
		Acquired bool
	}

	maxConc := cfg.Local.MaxConcurrent
	if maxConc <= 0 {
		maxConc = defaultMaxConcurrent
	}

	results := make([]chunkResult, len(chunks))
	sem := make(chan searchConcurrencyPermit, maxConc)
	var wg sync.WaitGroup

	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, msgs []transcript.Message) {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "sweep: chunk worker panic", "chunk", idx, "panic", r, "err", fmt.Errorf("panic: %v", r))
				}
			}()
			defer wg.Done()
			sem <- searchConcurrencyPermit{Acquired: true}
			defer func() { <-sem }()

			chunkStart := searchClock.Now()
			res, err := searchChunk(ctx, client, msgs, query)
			results[idx] = chunkResult{idx: idx, result: res, err: err}
			hit := res != nil && len(res.Messages) > 0
			log.DebugContext(ctx, "sweep: chunk done",
				"chunk", idx,
				"messages", len(msgs),
				"hit", hit,
				"duration", time.Since(chunkStart).Round(time.Millisecond),
			)
		}(i, chunk)
	}
	wg.Wait()

	var matched []Result
	for _, r := range results {
		if r.err != nil {
			continue
		}
		if r.result != nil && len(r.result.Messages) > 0 {
			matched = append(matched, *r.result)
		}
	}
	return matched
}

// deepAnalysis re-evaluates each result with a large model, asking it to
// verify relevance and provide a more detailed summary.
func deepAnalysis(ctx context.Context, client Client, results []Result, query string) []Result {
	var kept []Result
	for _, r := range results {
		// Build the conversation text from matched messages
		var convText strings.Builder
		for _, m := range r.Messages {
			ts := m.Timestamp.Format("2006-01-02 15:04")
			role := "User"
			if m.Role == "assistant" {
				role = "Assistant"
			}
			fmt.Fprintf(&convText, "[%s] %s:\n%s\n\n", ts, role, m.Text)
		}

		prompt := fmt.Sprintf(`You are verifying whether a conversation excerpt is relevant to a search query.

QUERY: %s

CONVERSATION EXCERPT:
%s

Is this conversation excerpt specifically relevant to the query?
If YES: respond with "YES" followed by a detailed 2-3 sentence summary of what was discussed and decided.
If NO: respond with exactly "NO".`, query, convText.String())

		resp, err := client.Complete(ctx, prompt)
		if err != nil {
			kept = append(kept, r) // on error, keep the result
			continue
		}

		resp = strings.TrimSpace(resp)
		if strings.HasPrefix(strings.ToUpper(resp), "NO") {
			continue
		}

		// Extract summary (everything after "YES" or "YES:")
		summary := resp
		if strings.HasPrefix(strings.ToUpper(summary), "YES") {
			summary = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(summary, "YES"), ":"))
		}
		if summary != "" {
			r.Summary = summary
		}
		kept = append(kept, r)
	}
	return kept
}

// rerankResults sends all result summaries to the LLM and asks which are
// genuinely relevant, filtering out false positives from the initial search.
func rerankResults(ctx context.Context, client Client, results []Result, query string) []Result {
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "[%d] %s\n", i, r.Summary)
	}

	prompt := fmt.Sprintf(`Given this search query and these result summaries, which results are relevant? Remove only clearly unrelated results.

QUERY: %s

RESULTS:
%s
Return ONLY the numbers of relevant results, one per line. If none are relevant, respond with NONE.`, query, sb.String())

	resp, err := client.Complete(ctx, prompt)
	if err != nil {
		return results // on error, return unfiltered
	}

	resp = strings.TrimSpace(resp)
	if resp == "NONE" || resp == "none" {
		return nil
	}

	// Normalize: models sometimes return comma-separated on one line instead of newline-separated
	normalized := strings.NewReplacer(",", "\n").Replace(resp)

	var filtered []Result
	seen := make(map[int]bool)
	for line := range strings.SplitSeq(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Parse bare number or [N] format
		var idx int
		if _, err := fmt.Sscanf(line, "[%d]", &idx); err != nil {
			_, _ = fmt.Sscanf(line, "%d", &idx)
		}
		if idx >= 0 && idx < len(results) && !seen[idx] {
			seen[idx] = true
			filtered = append(filtered, results[idx])
		}
	}
	if len(filtered) == 0 {
		return results // parsing failed, return unfiltered
	}
	return filtered
}

// searchChunk asks the LLM whether a conversation chunk discusses the query.
// If yes, returns the relevant messages. If no, returns nil.
func searchChunk(ctx context.Context, client Client, messages []transcript.Message, query string) (*Result, error) {
	// Build the conversation text for this chunk
	var convText strings.Builder
	for i, m := range messages {
		ts := m.Timestamp.Format("2006-01-02 15:04")
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		fmt.Fprintf(&convText, "[MSG %d][%s] %s:\n%s\n\n", i, ts, role, m.Text)
	}

	prompt := fmt.Sprintf(`You are searching a conversation transcript for a specific topic.

QUERY: %s

CONVERSATION:
%s

INSTRUCTIONS:
- Match if the conversation substantively discusses the query topic.
- Do NOT match on passing mentions or vague keyword overlap.
- If this conversation segment discusses the query topic, respond with the message numbers (MSG N) that are relevant, one per line:
MSG 0
MSG 3
MSG 4
- After the message numbers, add a blank line and a one-sentence summary.
- If this conversation does NOT discuss the query topic, respond with exactly: NO

Respond ONLY with message numbers + summary, or NO. Nothing else.`, query, convText.String())

	resp, err := client.Complete(ctx, prompt)
	if err != nil {
		return nil, err
	}

	resp = strings.TrimSpace(resp)
	if resp == "NO" || resp == "no" || resp == "No" {
		return nil, nil
	}

	// Parse message numbers from response
	lines := strings.Split(resp, "\n")
	var matchedMessages []transcript.Message
	var summary string
	pastNumbers := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			pastNumbers = true
			continue
		}
		if !pastNumbers {
			var msgIdx int
			if _, err := fmt.Sscanf(line, "MSG %d", &msgIdx); err == nil {
				if msgIdx >= 0 && msgIdx < len(messages) {
					matchedMessages = append(matchedMessages, messages[msgIdx])
				}
			}
		} else {
			summary = line
		}
	}

	if len(matchedMessages) == 0 {
		return nil, nil
	}

	return &Result{
		Messages: matchedMessages,
		Summary:  summary,
	}, nil
}

// chunkMessages splits messages into chunks of approximately maxChars each,
// splitting on message boundaries (never mid-message).
func chunkMessages(messages []transcript.Message, maxChars int) [][]transcript.Message {
	var chunks [][]transcript.Message
	var current []transcript.Message
	currentLen := 0

	for _, m := range messages {
		msgLen := len(m.Text) + 50 // overhead for timestamp/role
		if currentLen+msgLen > maxChars && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentLen = 0
		}
		current = append(current, m)
		currentLen += msgLen
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
