package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// scanCache is the prior scan's output, keyed by artifact path, that the next
// incremental scan reuses for unchanged files.
type scanCache struct {
	records map[string]Record
	stamps  map[string]FileStamp
}

// scanResult bundles the records a scan discovered with the file stamps it
// captured, which become the next scan's prior cache.
type scanResult struct {
	records []Record
	stamps  map[string]FileStamp
}

// scan discovers raw conversation artifacts through the registered parsers. For
// each provider it lists candidates with [Parser.Discover] and reuses any record
// in prior whose file stamp is unchanged, so only new or modified artifacts are
// re-read through [Parser.ScanRecord]. The size and mtime skip is the steady
// state that keeps the background index near idle.
func scan(ctx context.Context, registry *Registry, prior scanCache) (scanResult, error) {
	out := make([]Record, 0, len(prior.records))
	stamps := make(map[string]FileStamp, len(prior.stamps))
	for _, provider := range registry.Providers() {
		parser, err := registry.Lookup(provider)
		if err != nil {
			return scanResult{}, fmt.Errorf("lookup parser for %s: %w", provider.String(), err)
		}
		candidates, err := parser.Discover(ctx, prior.records)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			slog.WarnContext(ctx, "conversation.scan.discover_failed", "concern", "conversation.scan", "component", "conversation", "provider", provider.String(), "err", err)
			return scanResult{}, fmt.Errorf("discover %s conversations: %w", provider.String(), err)
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				slog.WarnContext(ctx, "conversation.scan.canceled", "concern", "conversation.scan", "component", "conversation", "provider", provider.String(), "err", ctx.Err())
				return scanResult{}, fmt.Errorf("scan %s conversations canceled: %w", provider.String(), ctx.Err())
			}
			stamps[candidate.Path] = candidate.Stamp
			if prev, ok := prior.stamps[candidate.Path]; ok && prev.Equal(candidate.Stamp) {
				// The artifact did not change. Reuse the prior record, or nothing
				// when the file previously yielded none, without re-reading it.
				if record, ok := prior.records[candidate.Path]; ok {
					out = append(out, record)
				}
				continue
			}
			if record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp); ok {
				out = append(out, record)
			}
		}
	}
	return scanResult{records: out, stamps: stamps}, nil
}
