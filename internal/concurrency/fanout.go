// Package concurrency holds small, reusable concurrency primitives shared
// across the cob codebase. Keeping them here instead of duplicating the
// scaffolding at each call site means there's one place to add
// cancellation, retry, or rate-limit telemetry later.
package concurrency

import (
	"context"
	"sync"
)

// ForEach runs fn against each item in items with at most limit goroutines
// in flight, returning results in input order. Cancellation of ctx stops
// scheduling new work; goroutines already running complete normally (each
// fn is expected to honor ctx itself for the long-running parts, typically
// by passing it into AWS SDK calls that abort on Done).
//
// fn returns one R per item — there is deliberately no error return.
// Per-item failures are carried inside R (an error field, a sentinel
// zero value the caller post-processes, or a degraded status the caller
// renders as "?"). This matches the best-effort-with-graceful-degradation
// shape of cob's ls / log / rm / registry fan-outs, where one slow or
// failing item must never fail the whole listing.
//
// limit <= 0 is treated as 1 (strictly sequential). The semaphore is
// acquired before the goroutine is spawned, so it bounds the goroutine
// count, not just in-flight work — important on a 5k-package ls.
func ForEach[T, R any](ctx context.Context, items []T, limit int, fn func(ctx context.Context, i int, item T) R) []R {
	out := make([]R, len(items))
	if len(items) == 0 {
		return out
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, it := range items {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, it T) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(ctx, i, it)
		}(i, it)
	}
	wg.Wait()
	return out
}
