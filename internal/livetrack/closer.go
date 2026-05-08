package livetrack

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// Closer is the contract every Registry-tracked owner provides for
// force-close. The reason argument identifies who triggered the
// force-close (drain deadline, predicate match, parent cascade). A
// Closer must be safe to call once and may assume the registry will
// not call it more than once per session.
type Closer interface {
	Close(reason string) error
}

// CloserFunc adapts a closure into a Closer. The closure receives
// the same reason string the registry passes to Close.
type CloserFunc func(reason string) error

func (f CloserFunc) Close(reason string) error {
	if f == nil {
		return nil
	}
	return f(reason)
}

// runCloserBounded invokes the closer for one session with a per-session
// timeout. Panics are recovered and converted into a returned error
// so sibling closers continue to run. The grace deadline ensures a
// wedged Close cannot block reload past its budget.
func runCloserBounded(grace time.Duration, sess sessionEntry, reason string) error {
	if sess.closer == nil {
		return nil
	}
	resultCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				stack := make([]byte, 4096)
				stack = stack[:runtime.Stack(stack, false)]
				slog.Error("livetrack.closer.panic",
					"component", "livetrack",
					"session_id", sess.id,
					"reason", reason,
					"panic", recovered,
					"err", fmt.Errorf("closer panic: %v", recovered),
				)
				resultCh <- fmt.Errorf("livetrack: closer panic for session %s: %v\n%s", sess.id, recovered, stack)
			}
		}()
		resultCh <- sess.closer.Close(reason)
	}()
	if grace <= 0 {
		return <-resultCh
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		return fmt.Errorf("livetrack: closer for session %s exceeded grace %s", sess.id, grace)
	}
}

// closerFanOut runs Close on every entry. When parallel is false, it
// runs sequentially in the supplied snapshot order. When parallel is
// true, it fans out across min(NumCPU, len(entries)) workers. Errors
// are joined via errors.Join so callers see exactly the failures
// produced. ctx is honored: once it expires, the fan-out stops
// scheduling new closers but already-running ones continue until
// their grace deadline.
func closerFanOut(ctx context.Context, parallel bool, grace time.Duration, entries []sessionEntry, reason string) []error {
	if len(entries) == 0 {
		return nil
	}
	if !parallel {
		return runCloserSequential(ctx, grace, entries, reason)
	}
	return runCloserParallel(ctx, grace, entries, reason)
}

func runCloserSequential(ctx context.Context, grace time.Duration, entries []sessionEntry, reason string) []error {
	errs := make([]error, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("livetrack: aborted before closing session %s: %w", entry.id, err))
			continue
		}
		if err := runCloserBounded(grace, entry, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func runCloserParallel(ctx context.Context, grace time.Duration, entries []sessionEntry, reason string) []error {
	workerCount := runtime.NumCPU()
	if workerCount > len(entries) {
		workerCount = len(entries)
	}
	if workerCount < 1 {
		workerCount = 1
	}
	jobs := make(chan sessionEntry, len(entries))
	for _, entry := range entries {
		jobs <- entry
	}
	close(jobs)
	errCh := make(chan error, len(entries))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.ErrorContext(ctx, "livetrack.closer.worker_panic",
						"component", "livetrack",
						"reason", reason,
						"panic", recovered,
						"err", fmt.Errorf("worker panic: %v", recovered),
					)
					errCh <- fmt.Errorf("livetrack: closer worker panic: %v", recovered)
				}
			}()
			for entry := range jobs {
				if err := ctx.Err(); err != nil {
					errCh <- fmt.Errorf("livetrack: aborted before closing session %s: %w", entry.id, err)
					continue
				}
				if err := runCloserBounded(grace, entry, reason); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	errs := make([]error, 0, len(entries))
	for err := range errCh {
		errs = append(errs, err)
	}
	return errs
}
