package cli

import (
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/jmurray2011/cob/pkg/cob"
)

func TestClampConcurrency(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 4: 4, 64: 64} {
		if got := clampConcurrency(in); got != want {
			t.Errorf("clampConcurrency(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestRunConcurrentOrderAndCompleteness(t *testing.T) {
	const n = 25
	var calls int32
	results, errIdx, ok := runConcurrent(n, 6, func(i int) (*cob.AssetResult, error) {
		atomic.AddInt32(&calls, 1)
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if !ok || errIdx != -1 {
		t.Fatalf("ok=%v errIdx=%d, want true/-1", ok, errIdx)
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
	results, errIdx, ok := runConcurrent(6, 1, func(i int) (*cob.AssetResult, error) {
		atomic.AddInt32(&calls, 1)
		if i == 3 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok || errIdx != 3 {
		t.Fatalf("ok=%v errIdx=%d, want false/3", ok, errIdx)
	}
	if calls != 4 { // 0,1,2,3 then stop
		t.Fatalf("called %d times, want 4 (must stop after the failure)", calls)
	}
	if results[4] != nil || results[5] != nil {
		t.Fatalf("post-failure tasks must not run: results[4]=%v results[5]=%v", results[4], results[5])
	}
}

func TestRunConcurrentParallelReportsMinErrorIndex(t *testing.T) {
	results, errIdx, ok := runConcurrent(20, 8, func(i int) (*cob.AssetResult, error) {
		if i == 5 {
			return &cob.AssetResult{}, errors.New("boom")
		}
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if ok || errIdx != 5 {
		t.Fatalf("ok=%v errIdx=%d, want false/5", ok, errIdx)
	}
	_ = results
}

func TestRunFinalizeProtocolUnfinishedFlags(t *testing.T) {
	const n = 6
	var flags [n]bool
	results, _, ok := runFinalizeProtocol(n, 3, func(i int, unfinished bool) (*cob.AssetResult, error) {
		flags[i] = unfinished // distinct index per call: race-free
		return &cob.AssetResult{Name: strconv.Itoa(i)}, nil
	})
	if !ok {
		t.Fatal("expected ok")
	}
	for i := 0; i < n-1; i++ {
		if !flags[i] {
			t.Errorf("asset %d should be unfinished=true", i)
		}
	}
	if flags[n-1] {
		t.Errorf("final asset %d must be unfinished=false (it flips the version to Published)", n-1)
	}
	for i := 0; i < n; i++ {
		if results[i] == nil || results[i].Name != strconv.Itoa(i) {
			t.Errorf("results[%d] out of order: %v", i, results[i])
		}
	}
}

func TestRunFinalizeProtocolSingleAsset(t *testing.T) {
	var got bool
	calls := 0
	_, _, ok := runFinalizeProtocol(1, 4, func(i int, unfinished bool) (*cob.AssetResult, error) {
		calls++
		got = unfinished
		return &cob.AssetResult{}, nil
	})
	if !ok || calls != 1 || got {
		t.Fatalf("single asset: ok=%v calls=%d unfinished=%v, want true/1/false", ok, calls, got)
	}
}

func TestRunFinalizeProtocolHeadFailureSkipsFinalize(t *testing.T) {
	const n = 5
	var finalizeCalled atomic.Bool
	_, errIdx, ok := runFinalizeProtocol(n, 2, func(i int, unfinished bool) (*cob.AssetResult, error) {
		if i == n-1 {
			finalizeCalled.Store(true)
		}
		if i == 1 {
			return &cob.AssetResult{}, errors.New("head boom")
		}
		return &cob.AssetResult{}, nil
	})
	if ok {
		t.Fatal("expected failure")
	}
	if errIdx != 1 {
		t.Fatalf("errIdx=%d, want 1", errIdx)
	}
	if finalizeCalled.Load() {
		t.Fatal("finalizing asset must NOT run when a head asset failed (version stays Unfinished)")
	}
}
