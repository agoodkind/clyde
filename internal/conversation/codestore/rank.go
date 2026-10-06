package codestore

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const rankingWorkerRows = 4096

type candidate struct {
	position int
	distance int
}

// The heap root is the worst retained candidate, ready for replacement.
type boundedRanking struct {
	items    []candidate
	rows     []row
	capacity int
}

func (ranking *boundedRanking) worse(left, right candidate) bool {
	if left.distance != right.distance {
		return left.distance > right.distance
	}
	return ranking.rows[left.position].id > ranking.rows[right.position].id
}

func (ranking *boundedRanking) offer(next candidate) {
	if len(ranking.items) < ranking.capacity {
		ranking.items = append(ranking.items, next)
		ranking.siftUp(len(ranking.items) - 1)
		return
	}
	if !ranking.worse(ranking.items[0], next) {
		return
	}
	ranking.items[0] = next
	ranking.siftDown(0)
}

func (ranking *boundedRanking) siftUp(index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !ranking.worse(ranking.items[index], ranking.items[parent]) {
			return
		}
		ranking.items[index], ranking.items[parent] = ranking.items[parent], ranking.items[index]
		index = parent
	}
}

func (ranking *boundedRanking) siftDown(index int) {
	for {
		worst := index
		for _, child := range []int{2*index + 1, 2*index + 2} {
			if child < len(ranking.items) && ranking.worse(ranking.items[child], ranking.items[worst]) {
				worst = child
			}
		}
		if worst == index {
			return
		}
		ranking.items[index], ranking.items[worst] = ranking.items[worst], ranking.items[index]
		index = worst
	}
}

// A worker panic invalidates the ranking from every worker.
func (stored *codeCollection) rank(query staticembed.Code, matches predicate) ([]candidate, error) {
	workers := max(min(runtime.GOMAXPROCS(0), len(stored.rows)/rankingWorkerRows), 1)
	chunk := (len(stored.rows) + workers - 1) / workers
	partials := make([]*boundedRanking, workers)
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
			best := &boundedRanking{items: make([]candidate, 0, 64), rows: stored.rows, capacity: collection.RankingDepth}
			start := worker * chunk
			end := min(start+chunk, len(stored.rows))
			for position := start; position < end; position++ {
				if matches(position) != truthTrue {
					continue
				}
				best.offer(candidate{position: position, distance: query.Distance(stored.codes[position])})
			}
			partials[worker] = best
		})
	}
	group.Wait()
	if panicked.Load() {
		return nil, failed("rank "+stored.dir, errors.New("a ranking worker panicked"))
	}
	merged := &boundedRanking{items: make([]candidate, 0), rows: stored.rows, capacity: collection.RankingDepth}
	for _, partial := range partials {
		merged.items = append(merged.items, partial.items...)
	}
	slices.SortFunc(merged.items, func(left, right candidate) int {
		if merged.worse(right, left) {
			return -1
		}
		if merged.worse(left, right) {
			return 1
		}
		return 0
	})
	if len(merged.items) > collection.RankingDepth {
		merged.items = merged.items[:collection.RankingDepth]
	}
	return merged.items, nil
}
