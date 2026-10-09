package codestore

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"goodkind.io/clyde/internal/conversation/staticembed"
)

const (
	rankingWorkerRows        = 4096
	selectionWindowFactor    = 2
	selectionMinimumCapacity = 64
)

type candidate struct {
	position int
	distance int
}

type rankingWalk struct {
	window      int
	maxDistance int
	grouped     bool
	groupColumn int
	perGroup    int32
}

func maxDistanceFor(minScore float64) int {
	limit := -1
	for distance := 0; distance <= staticembed.Dimensions; distance++ {
		if Score(distance) < minScore {
			break
		}
		limit = distance
	}
	return limit
}

func (stored *codeCollection) compareCandidates(left, right candidate) int {
	if left.distance != right.distance {
		return left.distance - right.distance
	}
	return strings.Compare(stored.rows[left.position].id, stored.rows[right.position].id)
}

func (stored *codeCollection) groupKey(position int, column int) uint32 {
	key := stored.rows[position].cells[column]
	if key != 0 && stored.dictionaries[column].values[key].Null {
		return 0
	}
	return key
}

func (stored *codeCollection) walkRanking(sorted []candidate, walk rankingWalk) []candidate {
	kept := sorted[:0]
	var perGroup map[uint32]int32
	if walk.grouped {
		perGroup = make(map[uint32]int32)
	}
	for _, ranked := range sorted {
		if len(kept) == walk.window {
			break
		}
		if walk.grouped {
			key := stored.groupKey(ranked.position, walk.groupColumn)
			if perGroup[key] >= walk.perGroup {
				continue
			}
			perGroup[key]++
		}
		kept = append(kept, ranked)
	}
	return kept
}

type selection struct {
	stored    *codeCollection
	walk      rankingWalk
	capacity  int
	items     []candidate
	full      bool
	threshold candidate
}

func (stored *codeCollection) newSelection(walk rankingWalk) *selection {
	return &selection{
		stored:    stored,
		walk:      walk,
		capacity:  max(walk.window*selectionWindowFactor, selectionMinimumCapacity),
		items:     make([]candidate, 0, selectionMinimumCapacity),
		full:      false,
		threshold: candidate{position: 0, distance: 0},
	}
}

func (selected *selection) offer(next candidate) {
	if selected.full && selected.stored.compareCandidates(next, selected.threshold) >= 0 {
		return
	}
	selected.items = append(selected.items, next)
	if len(selected.items) >= selected.capacity {
		selected.settle()
	}
}

func (selected *selection) settle() {
	slices.SortFunc(selected.items, selected.stored.compareCandidates)
	selected.items = selected.stored.walkRanking(selected.items, selected.walk)
	if len(selected.items) == selected.walk.window {
		selected.full = true
		selected.threshold = selected.items[len(selected.items)-1]
	}
}

// A worker panic invalidates the ranking from every worker.
func (stored *codeCollection) rank(query staticembed.Code, matches predicate, walk rankingWalk) ([]candidate, error) {
	workers := max(min(runtime.GOMAXPROCS(0), len(stored.rows)/rankingWorkerRows), 1)
	chunk := (len(stored.rows) + workers - 1) / workers
	partials := make([]*selection, workers)
	var panicked atomic.Bool
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					panicked.Store(true)
					slog.Error("conversation.codestore.rank_worker_panic",
						"concern", "conversation.semantic",
						"component", "conversation",
						"err", fmt.Errorf("panic: %v", recovered),
					)
				}
			}()
			best := stored.newSelection(walk)
			start := worker * chunk
			end := min(start+chunk, len(stored.rows))
			for position := start; position < end; position++ {
				if matches(position) != truthTrue {
					continue
				}
				distance := query.Distance(stored.codes[position])
				if distance > walk.maxDistance {
					continue
				}
				best.offer(candidate{position: position, distance: distance})
			}
			best.settle()
			partials[worker] = best
		})
	}
	group.Wait()
	if panicked.Load() {
		return nil, failed("rank "+stored.dir, errors.New("a ranking worker panicked"))
	}
	merged := make([]candidate, 0)
	for _, partial := range partials {
		merged = append(merged, partial.items...)
	}
	slices.SortFunc(merged, stored.compareCandidates)
	return stored.walkRanking(merged, walk), nil
}
