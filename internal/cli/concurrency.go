package cli

import (
	"sync"

	"github.com/jmurray2011/cob/pkg/cob"
)

const defaultConcurrency = 4

func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// runConcurrent runs task for indices [0,n) with at most `limit` in flight,
// returning results in index order. On the first error it stops scheduling
// new tasks (already-running ones finish); ok is false and firstErrIdx is
// the lowest index that failed. limit<=1 runs strictly sequentially, which
// is the exact pre-concurrency behaviour.
func runConcurrent(n, limit int, task func(i int) (*cob.AssetResult, error)) (results []*cob.AssetResult, firstErrIdx int, ok bool) {
	results = make([]*cob.AssetResult, n)
	if n == 0 {
		return results, -1, true
	}
	if clampConcurrency(limit) == 1 {
		for i := 0; i < n; i++ {
			r, err := task(i)
			results[i] = r
			if err != nil {
				return results, i, false
			}
		}
		return results, -1, true
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sem  = make(chan struct{}, limit)
		errI = -1
	)
	for i := 0; i < n; i++ {
		mu.Lock()
		stop := errI != -1
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
			if err != nil && (errI == -1 || i < errI) {
				errI = i
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if errI != -1 {
		return results, errI, false
	}
	return results, -1, true
}
