package cob

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

func coords() *PackageCoordinates {
	return &PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1.0.0"}
}

func TestCheckVersionExists(t *testing.T) {
	ctx := context.Background()

	t.Run("describe succeeds -> exists (incl. Unfinished, which the old "+
		"Published-filtered scan missed)", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{
					Version: aws.String("1.0.0"),
					Status:  catypes.PackageVersionStatusUnfinished,
				},
			}, nil
		}}
		got, err := NewRegistry(newTestClient(ca)).CheckVersionExists(ctx, coords())
		if err != nil || !got {
			t.Fatalf("got (%v,%v), want (true,nil)", got, err)
		}
	})

	t.Run("not found -> false, no error", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		got, err := NewRegistry(newTestClient(ca)).CheckVersionExists(ctx, coords())
		if err != nil || got {
			t.Fatalf("got (%v,%v), want (false,nil)", got, err)
		}
	})

	t.Run("other error propagates", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, errors.New("throttled")
		}}
		got, err := NewRegistry(newTestClient(ca)).CheckVersionExists(ctx, coords())
		if err == nil || got {
			t.Fatalf("got (%v,%v), want (false,err)", got, err)
		}
	})
}

func TestResolveLatest(t *testing.T) {
	ctx := context.Background()

	t.Run("returns first version", func(t *testing.T) {
		ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("2.0.0")}},
			}, nil
		}}
		v, err := NewRegistry(newTestClient(ca)).ResolveLatest(ctx, coords())
		if err != nil || v != "2.0.0" {
			t.Fatalf("got (%q,%v), want (2.0.0,nil)", v, err)
		}
	})

	t.Run("no versions -> error", func(t *testing.T) {
		ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{}, nil
		}}
		if _, err := NewRegistry(newTestClient(ca)).ResolveLatest(ctx, coords()); err == nil {
			t.Fatal("expected error for no versions")
		}
	})

	t.Run("not found -> error", func(t *testing.T) {
		ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		if _, err := NewRegistry(newTestClient(ca)).ResolveLatest(ctx, coords()); err == nil {
			t.Fatal("expected error for not found")
		}
	})
}

func TestVersionStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("returns real status", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
			}, nil
		}}
		st, found, err := NewRegistry(newTestClient(ca)).VersionStatus(ctx, coords())
		if err != nil || !found || st != string(catypes.PackageVersionStatusUnfinished) {
			t.Fatalf("got (%q,%v,%v), want (Unfinished,true,nil)", st, found, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		st, found, err := NewRegistry(newTestClient(ca)).VersionStatus(ctx, coords())
		if err != nil || found || st != "" {
			t.Fatalf("got (%q,%v,%v), want (\"\",false,nil)", st, found, err)
		}
	})
}

// TestListVersionsBestEffort: a per-version metadata error must NOT fail the
// whole listing — the failed row keeps zero Assets/Published, others still
// populate. (Regression guard for the A2/C3 fix.)
func TestListVersionsBestEffort(t *testing.T) {
	ctx := context.Background()
	order := []string{"1.0.3", "1.0.2", "1.0.1"}
	ca := &fakeCA{
		listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			var vs []catypes.PackageVersionSummary
			for _, v := range order {
				vs = append(vs, catypes.PackageVersionSummary{Version: aws.String(v)})
			}
			return &codeartifact.ListPackageVersionsOutput{Versions: vs}, nil
		},
		describeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			if aws.ToString(in.PackageVersion) == "1.0.2" {
				return nil, errors.New("throttled")
			}
			pt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{PublishedTime: &pt},
			}, nil
		},
		listAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			return &codeartifact.ListPackageVersionAssetsOutput{
				Assets: []catypes.AssetSummary{{Name: aws.String("a")}},
			}, nil
		},
	}

	got, err := NewRegistry(newTestClient(ca)).ListVersions(ctx, coords())
	if err != nil {
		t.Fatalf("ListVersions must not fail when one version errors: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d versions, want 3 (all listed despite one meta error)", len(got))
	}
	for i, v := range order {
		if got[i].Version != v {
			t.Fatalf("order: [%d]=%q want %q", i, got[i].Version, v)
		}
	}
	// 1.0.2 (index 1) failed -> zero values; the others populated.
	if got[1].Assets != 0 || !got[1].Published.IsZero() {
		t.Errorf("failed version should keep zero meta, got Assets=%d Published=%v", got[1].Assets, got[1].Published)
	}
	if got[0].Assets != 1 || got[2].Assets != 1 {
		t.Errorf("healthy versions should still populate: %+v / %+v", got[0], got[2])
	}
}

func TestListVersionsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
		return &codeartifact.ListPackageVersionsOutput{
			Versions: []catypes.PackageVersionSummary{{Version: aws.String("1.0.0")}},
		}, nil
	}}
	if _, err := NewRegistry(newTestClient(ca)).ListVersions(ctx, coords()); err == nil {
		t.Fatal("expected context cancellation to surface as an error")
	}
}

// TestListVersionsFanOut verifies the bounded-concurrency fan-out populates
// Assets + Published for every version and preserves the listing order.
func TestListVersionsFanOut(t *testing.T) {
	ctx := context.Background()
	order := []string{"1.0.3", "1.0.2", "1.0.1"}
	times := map[string]time.Time{
		"1.0.3": time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
		"1.0.2": time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		"1.0.1": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	assetCounts := map[string]int{"1.0.3": 3, "1.0.2": 2, "1.0.1": 1}

	ca := &fakeCA{
		listVersionsFn: func(in *codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			var vs []catypes.PackageVersionSummary
			for _, v := range order {
				vs = append(vs, catypes.PackageVersionSummary{Version: aws.String(v)})
			}
			return &codeartifact.ListPackageVersionsOutput{Versions: vs}, nil
		},
		describeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			v := aws.ToString(in.PackageVersion)
			pt := times[v]
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{PublishedTime: &pt},
			}, nil
		},
		listAssetsFn: func(in *codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			v := aws.ToString(in.PackageVersion)
			n := assetCounts[v]
			// Exercise the pagination loop for the 3-asset version.
			if v == "1.0.3" && in.NextToken == nil {
				return &codeartifact.ListPackageVersionAssetsOutput{
					Assets:    []catypes.AssetSummary{{Name: aws.String("a1")}, {Name: aws.String("a2")}},
					NextToken: aws.String("page2"),
				}, nil
			}
			if v == "1.0.3" {
				return &codeartifact.ListPackageVersionAssetsOutput{
					Assets: []catypes.AssetSummary{{Name: aws.String("a3")}},
				}, nil
			}
			as := make([]catypes.AssetSummary, n)
			for i := range as {
				as[i] = catypes.AssetSummary{Name: aws.String("x")}
			}
			return &codeartifact.ListPackageVersionAssetsOutput{Assets: as}, nil
		},
	}

	got, err := NewRegistry(newTestClient(ca)).ListVersions(ctx, coords())
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d versions, want 3", len(got))
	}
	for i, v := range order {
		if got[i].Version != v {
			t.Fatalf("order not preserved: [%d] = %q, want %q", i, got[i].Version, v)
		}
		if got[i].Assets != assetCounts[v] {
			t.Errorf("%s: Assets = %d, want %d", v, got[i].Assets, assetCounts[v])
		}
		if !got[i].Published.Equal(times[v]) {
			t.Errorf("%s: Published = %v, want %v", v, got[i].Published, times[v])
		}
	}
}

// TestListAssetsPaginationSafetyCap exercises the belt-and-braces page
// cap that protects every CodeArtifact paginator from a misbehaving SDK
// or proxy that returns a non-nil NextToken forever. The fake hands back
// a NextToken on every page; the call must error rather than loop or
// OOM. Asserts on ListAssets; the same shared maxPaginationIterations
// guard is wired into every CodeArtifact paginator (registry, source_ca,
// puller, promoter — the last two pinned by their own cap tests).
func TestListAssetsPaginationSafetyCap(t *testing.T) {
	var calls int
	ca := &fakeCA{listAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		calls++
		return &codeartifact.ListPackageVersionAssetsOutput{
			Assets:    []catypes.AssetSummary{{Name: aws.String("a")}},
			NextToken: aws.String("forever"),
		}, nil
	}}
	_, err := NewRegistry(newTestClient(ca)).ListAssets(context.Background(), coords())
	if err == nil {
		t.Fatal("expected pagination safety cap to error out")
	}
	if !strings.Contains(err.Error(), "pagination safety cap") {
		t.Errorf("error should mention the cap, got %v", err)
	}
	// One page over the cap (the +1 attempt that triggers the guard) is
	// fine; way over means the guard didn't actually engage.
	if calls > maxPaginationIterations+1 {
		t.Errorf("paginator made %d calls, expected at most %d (cap + 1)", calls, maxPaginationIterations+1)
	}
}
