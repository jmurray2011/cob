package concurrency

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestForEachPreservesOrder(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7}
	got := ForEach(context.Background(), items, 4, func(_ context.Context, _ int, v int) int {
		return v * 10
	})
	for i := range items {
		if got[i] != items[i]*10 {
			t.Errorf("ForEach[%d] = %d, want %d (input-order indexing must hold even with parallel execution)", i, got[i], items[i]*10)
		}
	}
}

func TestForEachBoundsConcurrency(t *testing.T) {
	// limit=3 must never let more than 3 goroutines run fn simultaneously,
	// regardless of input size. Each invocation bumps a counter on entry,
	// records its peak, decrements on exit; the test asserts peak <= limit.
	var inFlight, peak int32
	const limit = 3
	items := make([]int, 50)
	ForEach(context.Background(), items, limit, func(_ context.Context, _ int, _ int) struct{} {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if now <= p || atomic.CompareAndSwapInt32(&peak, p, now) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return struct{}{}
	})
	if peak > limit {
		t.Errorf("peak in-flight = %d, exceeds limit %d", peak, limit)
	}
}

func TestForEachStopsSchedulingOnCancel(t *testing.T) {
	// After cancel, no further fn invocations should start. Items processed
	// before cancel race the cancellation — they may or may not run — but
	// once ctx.Err() != nil the loop in ForEach must break, so the
	// invocation count is bounded by what was already in flight + queued
	// when cancel fired.
	ctx, cancel := context.WithCancel(context.Background())
	var started int32
	const limit = 2
	items := make([]int, 200)
	go func() {
		// Let a few items kick off, then cancel.
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	ForEach(ctx, items, limit, func(_ context.Context, _ int, _ int) struct{} {
		atomic.AddInt32(&started, 1)
		time.Sleep(1 * time.Millisecond)
		return struct{}{}
	})
	// We don't know the exact bound; we just know it must be well below
	// len(items) — otherwise cancellation wasn't honored at all.
	if started >= int32(len(items)) {
		t.Errorf("cancellation must stop scheduling; ran all %d items", started)
	}
}

func TestForEachEmptyInputReturnsEmpty(t *testing.T) {
	got := ForEach(context.Background(), []string{}, 4, func(_ context.Context, _ int, _ string) string {
		t.Fatal("fn must not run on empty input")
		return ""
	})
	if len(got) != 0 {
		t.Errorf("empty input should return empty result, got len=%d", len(got))
	}
}

func TestForEachZeroLimitFallsBackToSequential(t *testing.T) {
	// limit=0 (or negative) is treated as 1; documented so a missing config
	// value doesn't accidentally serialize-on-purpose pretending to fan out.
	var peak, inFlight int32
	ForEach(context.Background(), []int{1, 2, 3, 4, 5}, 0, func(_ context.Context, _ int, _ int) struct{} {
		n := atomic.AddInt32(&inFlight, 1)
		if n > atomic.LoadInt32(&peak) {
			atomic.StoreInt32(&peak, n)
		}
		time.Sleep(1 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return struct{}{}
	})
	if peak > 1 {
		t.Errorf("limit<=0 should serialize; peak=%d", peak)
	}
}
