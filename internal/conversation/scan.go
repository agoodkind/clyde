package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"sort"

	"goodkind.io/clyde/internal/providerid"
)

// scanCache is the prior scan's output. Records and stamps are keyed by artifact
// path plus selector, while multi-conversation scan state is keyed by artifact.
type scanCache struct {
	records     map[string]Record
	stamps      map[string]FileStamp
	multiStates map[string]MultiConversationScanState
}

// scanResult bundles the records a scan discovered with the file stamps it
// captured, which become the next scan's prior cache.
type scanResult struct {
	changed     bool
	records     []Record
	stamps      map[string]FileStamp
	multiStates map[string]MultiConversationScanState
}

// scan discovers raw conversation artifacts through the registered parsers. For
// each provider it lists candidates with [Parser.Discover] and reuses any record
// in prior whose file stamp is unchanged, so only new or modified artifacts are
// re-read through [Parser.ScanRecord] or [MultiConversationParser.ScanRecords].
// The size and mtime skip is the steady state that keeps the background index
// near idle.
func scan(ctx context.Context, registry *Registry, prior scanCache) (scanResult, error) {
	out := make([]Record, 0, len(prior.records))
	stamps := make(map[string]FileStamp, len(prior.stamps))
	multiStates := make(map[string]MultiConversationScanState, len(prior.multiStates))
	for _, provider := range registry.Providers() {
		parser, err := registry.Lookup(provider)
		if err != nil {
			return scanResult{}, fmt.Errorf("lookup parser for %s: %w", provider.String(), err)
		}
		candidates, err := discoverCandidates(ctx, parser, provider, prior)
		if err != nil {
			return scanResult{}, err
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				slog.WarnContext(ctx, "conversation.scan.canceled", "concern", "conversation.scan", "component", "conversation", "provider", provider.String(), "err", ctx.Err())
				return scanResult{}, fmt.Errorf("scan %s conversations canceled: %w", provider.String(), ctx.Err())
			}
			result := scanCandidate(ctx, parser, candidate, prior)
			out = append(out, result.records...)
			if result.multiState != nil {
				multiStates[candidate.Path] = *result.multiState
			}
			if result.stampRecorded {
				if len(result.records) == 0 {
					stamps[recordKey(candidate.Path, candidate.Selector)] = candidate.Stamp
					continue
				}
				for _, record := range result.records {
					stamps[recordKey(record.ArtifactPath, record.Selector)] = candidate.Stamp
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return scanResult{}, fmt.Errorf("scan conversations: %w", err)
	}
	changed := prior.stamps == nil ||
		!maps.EqualFunc(prior.records, recordsByPath(out), func(a, b Record) bool { return reflect.DeepEqual(a, b) }) ||
		!maps.EqualFunc(prior.stamps, stamps, FileStamp.Equal) ||
		!maps.EqualFunc(prior.multiStates, multiStates, func(a, b MultiConversationScanState) bool {
			return a.Stamp.Equal(b.Stamp) && a.CompleteOffset == b.CompleteOffset
		})
	return scanResult{changed: changed, records: out, stamps: stamps, multiStates: multiStates}, nil
}

func discoverCandidates(
	ctx context.Context,
	parser Parser,
	provider providerid.Provider,
	prior scanCache,
) ([]ScanCandidate, error) {
	var (
		candidates []ScanCandidate
		err        error
	)
	if cached, ok := parser.(CachedDiscoveryParser); ok {
		candidates, err = cached.DiscoverCached(ctx, prior.records, prior.stamps)
	} else {
		candidates, err = parser.Discover(ctx, prior.records)
	}
	if err == nil {
		return candidates, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	slog.WarnContext(ctx, "conversation.scan.discover_failed", "concern", "conversation.scan", "component", "conversation", "provider", provider.String(), "err", err)
	return nil, fmt.Errorf("discover %s conversations: %w", provider.String(), err)
}

type scanCandidateResult struct {
	records       []Record
	stampRecorded bool
	multiState    *MultiConversationScanState
}

func scanCandidate(ctx context.Context, parser Parser, candidate ScanCandidate, prior scanCache) scanCandidateResult {
	if multi, ok := parser.(MultiConversationParser); ok {
		return scanMultiConversationCandidate(ctx, multi, candidate, prior)
	}
	key := recordKey(candidate.Path, candidate.Selector)
	if previous, ok := prior.stamps[key]; ok && previous.Equal(candidate.Stamp) && !candidate.MetadataChanged {
		record, found := prior.records[key]
		if found {
			return scanCandidateResult{records: []Record{record}, stampRecorded: true, multiState: nil}
		}
		return scanCandidateResult{records: nil, stampRecorded: true, multiState: nil}
	}
	if record, found := parser.ScanRecord(candidate.Path, candidate.Stamp); found {
		return scanCandidateResult{records: []Record{record}, stampRecorded: true, multiState: nil}
	}
	if record, found := prior.records[key]; found {
		return scanCandidateResult{records: []Record{record}, stampRecorded: false, multiState: nil}
	}
	return scanCandidateResult{records: nil, stampRecorded: true, multiState: nil}
}

func scanMultiConversationCandidate(
	ctx context.Context,
	parser MultiConversationParser,
	candidate ScanCandidate,
	prior scanCache,
) scanCandidateResult {
	priorRecords := recordsForArtifact(prior.records, candidate.Path)
	state, stateFound := prior.multiStates[candidate.Path]
	if stateFound && state.Stamp.Equal(candidate.Stamp) && !candidate.MetadataChanged {
		return scanCandidateResult{
			records:       priorRecords,
			stampRecorded: true,
			multiState:    &state,
		}
	}

	input := MultiConversationScan{
		Candidate:    candidate,
		PriorRecords: nil,
		StartOffset:  0,
	}
	if stateFound && !candidate.MetadataChanged && candidate.Stamp.Size > state.Stamp.Size &&
		state.CompleteOffset >= 0 && state.CompleteOffset <= state.Stamp.Size {
		input.PriorRecords = priorRecords
		input.StartOffset = state.CompleteOffset
	}
	result, found := parser.ScanRecords(ctx, input)
	if found {
		currentState := MultiConversationScanState{
			Stamp:          candidate.Stamp,
			CompleteOffset: result.CompleteOffset,
		}
		return scanCandidateResult{
			records:       result.Records,
			stampRecorded: true,
			multiState:    &currentState,
		}
	}
	if len(priorRecords) > 0 {
		return scanCandidateResult{
			records:       priorRecords,
			stampRecorded: false,
			multiState:    nil,
		}
	}
	return scanCandidateResult{records: nil, stampRecorded: true, multiState: nil}
}

func recordsForArtifact(records map[string]Record, path string) []Record {
	grouped := make([]Record, 0)
	for _, record := range records {
		if record.ArtifactPath == path {
			grouped = append(grouped, record)
		}
	}
	sort.SliceStable(grouped, func(i int, j int) bool {
		if grouped[i].Selector != grouped[j].Selector {
			return grouped[i].Selector < grouped[j].Selector
		}
		return grouped[i].ID < grouped[j].ID
	})
	return grouped
}

func recordKey(path string, selector string) string {
	if selector == "" {
		return path
	}
	return path + "\x00" + selector
}
