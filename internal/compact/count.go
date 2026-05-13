// DebugTokenCounter is non-authoritative; the planner uses
// contextusage.CandidateProber as the source of truth. This counter
// exists only for the optional debug cross-check gated by
// CLYDE_COMPACT_DEBUG_COUNT_TOKENS=1.

package compact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
)

// CountTokensEndpoint is the Anthropic Messages count_tokens endpoint.
// Override only in tests via NewDebugTokenCounter. Used solely by the
// optional debug cross-check counter; the planner itself does not call
// this endpoint.
const CountTokensEndpoint = "https://api.anthropic.com/v1/messages/count_tokens"

// AnthropicVersion is the API version header sent with every request.
// Bumped only when Anthropic deprecates the older value.
const AnthropicVersion = "2023-06-01"

// DebugTokenCounter posts to Anthropic's count_tokens endpoint with a
// single user message whose content is the synthesized content array.
// It is non-authoritative: the planner uses
// contextusage.CandidateProber as the source of truth, and this
// counter is wired in only when CLYDE_COMPACT_DEBUG_COUNT_TOKENS=1.
type DebugTokenCounter struct {
	APIKey   string
	Endpoint string
	Model    string
	Client   *http.Client
}

// NewDebugTokenCounter builds a debug counter that posts to the
// production endpoint with a 60s timeout. apiKey is required and never
// logged. The result is non-authoritative.
func NewDebugTokenCounter(apiKey, model string) *DebugTokenCounter {
	return &DebugTokenCounter{
		APIKey:   apiKey,
		Endpoint: CountTokensEndpoint,
		Model:    model,
		Client:   &http.Client{Timeout: 60 * time.Second},
	}
}

// CountResponse is the parsed response payload. Anthropic returns
// only input_tokens for count_tokens.
type CountResponse struct {
	InputTokens int `json:"input_tokens"`
}

// maxRateLimitRetries caps how many times CountSyntheticUser retries a
// 429 response. At the 100 req/min default, the planner's target loop
// easily exceeds the limit on long transcripts; backing off here keeps
// the orchestrator simple.
const maxRateLimitRetries = 6

// CountSyntheticUser counts tokens for a single user message whose
// content is the supplied content-array. Mirrors the exact shape the
// orchestrator will append to the JSONL so the count is honest. On
// HTTP 429 the call honors Retry-After (or falls back to exponential
// backoff) and retries up to maxRateLimitRetries.
func (c *DebugTokenCounter) CountSyntheticUser(ctx context.Context, contentArray []OutputBlock) (int, error) {
	log := gklog.LoggerFromContext(ctx).With("component", "compact", "subcomponent", "debug_count_tokens")
	if c.APIKey == "" {
		log.LogAttrs(ctx, slog.LevelError, "compact.debug_count_tokens.skipped",
			slog.String("reason", "missing_api_key"),
			slog.String("err", "missing_api_key"),
		)
		return 0, fmt.Errorf("debug_count_tokens: missing API key")
	}
	if c.Model == "" {
		log.LogAttrs(ctx, slog.LevelError, "compact.debug_count_tokens.skipped",
			slog.String("reason", "missing_model"),
			slog.String("err", "missing_model"),
		)
		return 0, fmt.Errorf("debug_count_tokens: missing model")
	}
	type countMessage struct {
		Role    string        `json:"role"`
		Content []OutputBlock `json:"content"`
	}
	type countBody struct {
		Model    string         `json:"model"`
		Messages []countMessage `json:"messages"`
	}
	body := countBody{
		Model:    c.Model,
		Messages: []countMessage{{Role: "user", Content: contentArray}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "compact.debug_count_tokens.encode_failed",
			slog.String("model", c.Model),
			slog.Any("err", err),
		)
		return 0, fmt.Errorf("debug_count_tokens: encode body: %w", err)
	}

	var attempt int
	for {
		attempt++
		started := compactClock.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(encoded))
		if err != nil {
			log.LogAttrs(ctx, slog.LevelError, "compact.debug_count_tokens.request_build_failed",
				slog.String("model", c.Model),
				slog.Any("err", err),
			)
			return 0, fmt.Errorf("debug_count_tokens: build request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", AnthropicVersion)

		resp, err := c.Client.Do(req)
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.http_failed",
				slog.String("model", c.Model),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
				slog.Int("attempt", attempt),
				slog.Any("err", err),
			)
			return 0, fmt.Errorf("debug_count_tokens: HTTP: %w", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.response_close_failed",
				slog.String("model", c.Model),
				slog.Any("err", err),
			)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= maxRateLimitRetries {
				log.LogAttrs(ctx, slog.LevelError, "compact.debug_count_tokens.rate_limited_giving_up",
					slog.String("model", c.Model),
					slog.Int("attempt", attempt),
					slog.String("body_excerpt", truncateForError(respBody)),
					slog.String("err", "rate_limited"),
				)
				return 0, fmt.Errorf("debug_count_tokens: status 429 after %d retries: %s", attempt, truncateForError(respBody))
			}
			wait := parseRetryAfter(resp.Header.Get("Retry-After"))
			if wait == 0 {
				wait = backoffFor(attempt)
			}
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.rate_limited",
				slog.String("model", c.Model),
				slog.Int("attempt", attempt),
				slog.Int64("wait_ms", wait.Milliseconds()),
				slog.String("retry_after_header", resp.Header.Get("Retry-After")),
			)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.http_bad_status",
				slog.String("model", c.Model),
				slog.Int("status_code", resp.StatusCode),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
				slog.String("body_excerpt", truncateForError(respBody)),
			)
			return 0, fmt.Errorf("debug_count_tokens: status %d: %s", resp.StatusCode, truncateForError(respBody))
		}
		var parsed CountResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.decode_failed",
				slog.String("model", c.Model),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
				slog.Any("err", err),
			)
			return 0, fmt.Errorf("debug_count_tokens: decode response: %w", err)
		}
		if parsed.InputTokens <= 0 {
			log.LogAttrs(ctx, slog.LevelWarn, "compact.debug_count_tokens.zero_tokens",
				slog.String("model", c.Model),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			)
			return 0, fmt.Errorf("debug_count_tokens: zero input_tokens in response")
		}
		log.LogAttrs(ctx, slog.LevelDebug, "compact.debug_count_tokens.completed",
			slog.String("model", c.Model),
			slog.Int("input_token_count", parsed.InputTokens),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.Int("attempt", attempt),
		)
		return parsed.InputTokens, nil
	}
}

// parseRetryAfter reads the Retry-After response header and returns a
// duration. Supports both the integer-seconds form and the HTTP-date
// form. Returns zero when the header is absent or unparseable so the
// caller falls back to exponential backoff.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	// Integer seconds is the common shape Anthropic emits.
	var secs int
	if _, err := fmt.Sscanf(h, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// HTTP-date form.
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoffFor returns the wait duration when the Retry-After header is
// missing. 500ms, 1s, 2s, 4s, 8s, 15s pattern gives the 429 window
// time to settle on a 100 req/min budget.
func backoffFor(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 500 * time.Millisecond
	case 2:
		return 1 * time.Second
	case 3:
		return 2 * time.Second
	case 4:
		return 4 * time.Second
	case 5:
		return 8 * time.Second
	default:
		return 15 * time.Second
	}
}

// truncateForError keeps API error bodies short so they fit cleanly in
// terminal output without dumping multi-KB stack traces.
func truncateForError(b []byte) string {
	const maxLen = 400
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "..."
}
