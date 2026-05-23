package cli

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func TestRunConcurrentEnforcesCeiling(t *testing.T) {
	const n = 200
	var inFlight, peak int32
	cliutil.RunConcurrent(context.Background(), n, 5000, func(context.Context, int) (*cob.AssetResult, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return &cob.AssetResult{}, nil
	})
	if peak > cliutil.MaxConcurrency {
		t.Fatalf("peak concurrency %d exceeded ceiling %d (--concurrency must be clamped)", peak, cliutil.MaxConcurrency)
	}
}

func TestClampConcurrency(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 4: 4, 32: 32, 5000: 32} {
		if got := cliutil.ClampConcurrency(in); got != want {
			t.Errorf("cliutil.ClampConcurrency(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestRunConcurrentOrderAndCompleteness(t *testing.T) {
	const n = 25
	var calls int32
	results, ok := cliutil.RunConcurrent(context.Background(), n, 6, func(_ context.Context, i int) (*cob.AssetResult, error) {
		atomic.AddInt32(&calls, 1)
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if !ok {
		t.Fatal("expected ok")
	}
	if calls != n {
		t.Fatalf("called %d times, want %d", calls, n)
	}
	for i := 0; i < n; i++ {
		if results[i] == nil || results[i].Name != strconv.Itoa(i) {
			t.Fatalf("results[%d] = %v, want Name=%d (order must be preserved)", i, results[i], i)
		}
	}
}

func TestRunConcurrentSequentialStopsAtFirstError(t *testing.T) {
	// With the errgroup migration, all N g.Go calls get queued; the
	// short-circuit on first error happens via gctx cancellation, which
	// only matters for tasks that respect ctx — that's the contract for
	// real callers (publish/promote/pull/diff all pass ctx to AWS SDK
	// calls which honor it). This test models that: each task checks
	// gctx.Done before incrementing the call counter, so a goroutine
	// scheduled after the i=3 error sees the canceled ctx and bails
	// without touching the counter. The observable behavior matches the
	// pre-errgroup version: 4 calls (0,1,2,3 ran fully; 3 then errored
	// and canceled gctx; 4,5 saw the cancel and skipped).
	var calls int32
	_, ok := cliutil.RunConcurrent(context.Background(), 6, 1, func(gctx context.Context, i int) (*cob.AssetResult, error) {
		if gctx.Err() != nil {
			return nil, gctx.Err()
		}
		atomic.AddInt32(&calls, 1)
		if i == 3 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok {
		t.Fatal("expected failure")
	}
	if calls != 4 { // 0,1,2,3 then ctx-canceled
		t.Fatalf("called %d times, want 4 (post-failure tasks must respect gctx and bail before incrementing)", calls)
	}
}

func TestRunConcurrentParallelFailureReportsNotOK(t *testing.T) {
	_, ok := cliutil.RunConcurrent(context.Background(), 20, 8, func(_ context.Context, i int) (*cob.AssetResult, error) {
		if i == 5 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok {
		t.Fatal("a failed task must make the whole run not-ok")
	}
}

// TestRunConcurrentFirstErrorCancelsInFlightTasks pins the ctx-aware
// behavior added in C8: when one task errors, the wrapped ctx is
// canceled so peers that respect ctx (the AWS SDK is the production
// case; here, a select-on-ctx.Done sleep) shortcut instead of running
// the answer-discarded full duration.
func TestRunConcurrentFirstErrorCancelsInFlightTasks(t *testing.T) {
	var aborted int32
	// 8 tasks; task 0 fails fast. The other 7 sleep up to a second,
	// returning early when ctx is canceled. Without the new cancel
	// signal, they'd each run the full sleep — we'd see the test take
	// ~1s wall time. With cancellation wired, they all bail in
	// milliseconds and aborted should land at 7.
	start := time.Now()
	_, ok := cliutil.RunConcurrent(context.Background(), 8, 8, func(ctx context.Context, i int) (*cob.AssetResult, error) {
		if i == 0 {
			return &cob.AssetResult{}, errors.New("first to cliutil.Fail")
		}
		select {
		case <-ctx.Done():
			atomic.AddInt32(&aborted, 1)
			return &cob.AssetResult{}, ctx.Err()
		case <-time.After(time.Second):
			return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
		}
	})
	if ok {
		t.Fatal("first-error run must be not-ok")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("in-flight peers did not honor first-error cancel (elapsed %v) — cliutil.RunConcurrent's cancel signal isn't wiring through", elapsed)
	}
	if aborted < 1 {
		t.Errorf("expected at least one peer to observe ctx.Done() and abort; aborted=%d", aborted)
	}
}

// TestRunConcurrentRespectsAlreadyCanceledCtx: a ctx canceled before
// cliutil.RunConcurrent starts must not schedule any work.
func TestRunConcurrentRespectsAlreadyCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	_, ok := cliutil.RunConcurrent(ctx, 10, 4, func(context.Context, int) (*cob.AssetResult, error) {
		atomic.AddInt32(&calls, 1)
		return &cob.AssetResult{}, nil
	})
	if ok {
		t.Error("ok should be false when ctx is canceled before any task runs")
	}
	if calls > 1 {
		// 0 or 1 is acceptable — the very first iteration might race the cancel.
		t.Errorf("scheduled %d tasks against a canceled ctx; expected at most 1 (the loop's first check)", calls)
	}
}
