package cli

import (
	"context"
	"sync"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

const (
	defaultConcurrency = 4
	maxConcurrency     = 32 // ceiling: more just invites API throttling
)

func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxConcurrency {
		return maxConcurrency
	}
	return n
}

// resolveConcurrency clamps a user-supplied --concurrency value to the
// supported range and warns if it had to change, so a typo'd or unreasonable
// number is not silently coerced.
func resolveConcurrency(requested int, out *output.Writer) int {
	c := clampConcurrency(requested)
	if c != requested {
		out.Warn("--concurrency %d out of range [1,%d], using %d", requested, maxConcurrency, c)
	}
	return c
}

// runConcurrent runs task for indices [0,n) with at most `limit` in flight,
// returning results in index order. ctx is wrapped with a cancel that
// fires on the first error so in-flight tasks see the cancellation and
// can shortcut (AWS SDK calls abort, io.Copy bails) rather than running
// to completion against an answer the caller is about to discard. The
// task closure receives the wrapped ctx — not the outer one — so it must
// pass that through to any SDK / context-aware call.
//
// On the first error it stops scheduling new tasks (already-running
// ones finish) and reports ok=false. limit is clamped to
// [1,maxConcurrency]; a clamped value of 1 runs strictly sequentially,
// the exact pre-concurrency behaviour.
func runConcurrent(ctx context.Context, n, limit int, task func(ctx context.Context, i int) (*cob.AssetResult, error)) (results []*cob.AssetResult, ok bool) {
	results = make([]*cob.AssetResult, n)
	if n == 0 {
		return results, true
	}
	limit = clampConcurrency(limit)
	if limit == 1 {
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				return results, false
			}
			r, err := task(ctx, i)
			results[i] = r
			if err != nil {
				return results, false
			}
		}
		return results, true
	}

	// Derive a cancelable ctx so the first error proactively aborts
	// in-flight goroutines. defer cancel() also collapses the timer
	// goroutine on normal completion.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		sem    = make(chan struct{}, limit)
		failed bool
	)
	for i := 0; i < n; i++ {
		mu.Lock()
		stop := failed
		mu.Unlock()
		if stop {
			break
		}
		if ctx.Err() != nil {
			// Ctx canceled before this task ran. Mark failed so ok=false
			// reflects "we didn't complete the batch" — otherwise an
			// already-canceled ctx with zero scheduled tasks would report
			// ok=true, which lies about the caller's intent.
			mu.Lock()
			failed = true
			mu.Unlock()
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := task(ctx, i)
			mu.Lock()
			results[i] = r
			if err != nil {
				failed = true
				cancel() // signal in-flight peers to abort
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	return results, !failed
}
