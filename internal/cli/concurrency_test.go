package cli

import (
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
)

func TestRunConcurrentEnforcesCeiling(t *testing.T) {
	const n = 200
	var inFlight, peak int32
	runConcurrent(n, 5000, func(int) (*cob.AssetResult, error) {
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
	if peak > maxConcurrency {
		t.Fatalf("peak concurrency %d exceeded ceiling %d (--concurrency must be clamped)", peak, maxConcurrency)
	}
}

func TestClampConcurrency(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 4: 4, 32: 32, 5000: 32} {
		if got := clampConcurrency(in); got != want {
			t.Errorf("clampConcurrency(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestRunConcurrentOrderAndCompleteness(t *testing.T) {
	const n = 25
	var calls int32
	results, ok := runConcurrent(n, 6, func(i int) (*cob.AssetResult, error) {
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
	var calls int32
	results, ok := runConcurrent(6, 1, func(i int) (*cob.AssetResult, error) {
		atomic.AddInt32(&calls, 1)
		if i == 3 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok {
		t.Fatal("expected failure")
	}
	if calls != 4 { // 0,1,2,3 then stop
		t.Fatalf("called %d times, want 4 (must stop after the failure)", calls)
	}
	if results[4] != nil || results[5] != nil {
		t.Fatalf("post-failure tasks must not run: results[4]=%v results[5]=%v", results[4], results[5])
	}
}

func TestRunConcurrentParallelFailureReportsNotOK(t *testing.T) {
	_, ok := runConcurrent(20, 8, func(i int) (*cob.AssetResult, error) {
		if i == 5 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok {
		t.Fatal("a failed task must make the whole run not-ok")
	}
}
