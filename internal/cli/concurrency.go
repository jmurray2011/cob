package cli

import (
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
// returning results in index order. On the first error it stops scheduling
// new tasks (already-running ones finish) and reports ok=false. limit is
// clamped to [1,maxConcurrency]; a clamped value of 1 runs strictly
// sequentially, the exact pre-concurrency behaviour.
func runConcurrent(n, limit int, task func(i int) (*cob.AssetResult, error)) (results []*cob.AssetResult, ok bool) {
	results = make([]*cob.AssetResult, n)
	if n == 0 {
		return results, true
	}
	limit = clampConcurrency(limit)
	if limit == 1 {
		for i := 0; i < n; i++ {
			r, err := task(i)
			results[i] = r
			if err != nil {
				return results, false
			}
		}
		return results, true
	}

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
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := task(i)
			mu.Lock()
			results[i] = r
			if err != nil {
				failed = true
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	return results, !failed
}
