package cli

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// TestWalkBoundsGoroutines pins that a wide walk keeps the live goroutine
// count near treeWalkConcurrency instead of standing up one goroutine per
// tree node. The fan-out used to bound only in-flight API calls (a
// semaphore) while spawning an unbounded goroutine per child, so a walk over
// a large org could create one goroutine per node all at once.
//
// We block ListPackages, which only the per-repo expansion calls — the
// metadata-enrichment fan-outs (populatePackageMeta/populateVersionMeta) hit
// ListVersions/ListAssets, not ListPackages, and would otherwise trip the
// barrier from their own already-bounded pool. With n repos blocked at
// ListPackages, the goroutine delta tells bounded (~treeWalkConcurrency)
// from unbounded (~n) fan-out. Deterministic: the barrier is the sync point.
func TestWalkBoundsGoroutines(t *testing.T) {
	const n = 300
	repos := make([]catypes.RepositorySummary, n)
	for i := range repos {
		repos[i] = catypes.RepositorySummary{Name: aws.String(fmt.Sprintf("repo-%d", i))}
	}

	var inFlight int64
	release := make(chan struct{})
	ca := &clitest.FakeCA{
		ListDomainsFn: func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error) {
			return &codeartifact.ListDomainsOutput{Domains: []catypes.DomainSummary{{Name: aws.String("acme")}}}, nil
		},
		ListReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: repos}, nil
		},
		ListPackagesFn: func(*codeartifact.ListPackagesInput) (*codeartifact.ListPackagesOutput, error) {
			atomic.AddInt64(&inFlight, 1)
			<-release // hold the listing open so concurrent per-repo walkers pile up
			return &codeartifact.ListPackagesOutput{Packages: []catypes.PackageSummary{
				{Namespace: aws.String("tools"), Package: aws.String("app")},
			}}, nil
		},
		// Reached only after release, during best-effort package metadata.
		ListVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{}, nil
		},
	}

	registry := cob.NewRegistry(&cob.Client{CodeArtifact: ca})
	start := &cob.PackageCoordinates{Domain: "acme"} // walk the repos under one domain

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		walkHierarchy(context.Background(), registry, start, DepthPackages)
		close(done)
	}()

	// Wait until treeWalkConcurrency package listings are blocked — every
	// scheme caps in-flight API calls there, so this is reached either way.
	for atomic.LoadInt64(&inFlight) < int64(treeWalkConcurrency) {
		runtime.Gosched()
	}
	// Take the peak goroutine delta while the listings are held open.
	peak := 0
	for i := 0; i < 2000; i++ {
		if d := runtime.NumGoroutine() - before; d > peak {
			peak = d
		}
		runtime.Gosched()
	}
	close(release)
	<-done

	// Bounded fan-out parks ~treeWalkConcurrency workers plus a little
	// scaffolding; the old per-child model parked ~n. A threshold well above
	// the bound but far below n cleanly separates the two.
	if limit := 4 * treeWalkConcurrency; peak > limit {
		t.Errorf("walk held %d goroutines (delta from %d), want <= %d (treeWalkConcurrency=%d, repos=%d) — fan-out is unbounded",
			peak, before, limit, treeWalkConcurrency, n)
	}
}
