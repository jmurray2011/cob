package cliutil

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

const (
	DefaultConcurrency = 4
	MaxConcurrency     = 32 // ceiling: more just invites API throttling
)

// ClampConcurrency forces n into [1, MaxConcurrency].
func ClampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxConcurrency {
		return MaxConcurrency
	}
	return n
}

// ResolveConcurrency clamps a user-supplied --concurrency value to the
// supported range and warns if it had to change, so a typo'd or unreasonable
// number is not silently coerced.
func ResolveConcurrency(requested int, out *output.Writer) int {
	c := ClampConcurrency(requested)
	if c != requested {
		out.Warn("--concurrency %d out of range [1,%d], using %d", requested, MaxConcurrency, c)
	}
	return c
}

// RunConcurrent runs task for indices [0,n) with at most `limit` in flight,
// returning results in index order. Backed by golang.org/x/sync/errgroup:
//
//   - errgroup.WithContext returns a ctx that is canceled on the first
//     non-nil error, so in-flight peers see the cancellation through
//     their task's ctx and can shortcut (AWS SDK calls abort, io.Copy
//     bails) rather than running to completion against an answer the
//     caller is about to discard.
//   - SetLimit(limit) caps in-flight goroutines — g.Go blocks until a
//     slot opens, the same back-pressure shape the old hand-rolled
//     channel-semaphore + WaitGroup produced.
//
// limit is clamped to [1,MaxConcurrency]; tasks must honor the passed
// ctx to get the early-abort benefit (a task that ignores ctx will run
// to completion even after a peer errored).
//
// Behavior note vs the pre-errgroup implementation: that version
// short-circuited the scheduling loop on first error, so subsequent
// indices never even got their goroutine spawned. With errgroup all N
// g.Go calls eventually issue (gated by SetLimit) — but tasks scheduled
// after cancellation see the canceled ctx immediately and exit, so the
// observable behavior is the same for any ctx-respecting task. The
// trade is ~one cheap goroutine per remaining index for the win of
// shipping a 50-line hand-rolled fan-out in 8 lines built on the
// canonical primitive.
func RunConcurrent(ctx context.Context, n, limit int, task func(ctx context.Context, i int) (*cob.AssetResult, error)) (results []*cob.AssetResult, ok bool) {
	results = make([]*cob.AssetResult, n)
	if n == 0 {
		return results, true
	}
	// Pre-flight ctx check so a caller who passed in an already-canceled
	// ctx gets ok=false without spawning any goroutines. errgroup by
	// itself would queue all N g.Go calls; their tasks would then have
	// to notice the canceled ctx and return non-nil for g.Wait to error.
	// Tasks that ignore ctx (rare, but the API permits it) would
	// otherwise return ok=true against a clearly-failed batch.
	if err := ctx.Err(); err != nil {
		return results, false
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ClampConcurrency(limit))
	for i := 0; i < n; i++ {
		i := i // capture before goroutine
		g.Go(func() error {
			r, err := task(gctx, i)
			results[i] = r // unique index per goroutine — no mutex needed
			return err     // first non-nil cancels gctx via errgroup
		})
	}
	return results, g.Wait() == nil
}
