package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
)

// unfinishedCA returns a fakeCA whose DescribePackageVersion reports the
// version as Unfinished — the cleanup case that rm allows by default.
func unfinishedCA(deleted *int) *fakeCA {
	return &fakeCA{
		describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
			}, nil
		},
		deleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			if deleted != nil {
				*deleted++
			}
			return &codeartifact.DeletePackageVersionsOutput{}, nil
		},
	}
}

// publishedCA returns a fakeCA whose DescribePackageVersion reports the
// version as Published (a real release). Used to verify the Tier 1 gate.
// downstreamRepos, if non-nil, makes the version exist in *those* repos
// too (matched against input.Repository) — every other repo gets a
// ResourceNotFound. ListRepositoriesInDomain returns every name in
// allRepos.
func publishedCA(deleted *int, allRepos, downstreamRepos []string) *fakeCA {
	dn := make(map[string]bool, len(downstreamRepos))
	for _, r := range downstreamRepos {
		dn[r] = true
	}
	return &fakeCA{
		describeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			repo := aws.ToString(in.Repository)
			// Probes for the deleted version's home repo and any downstream
			// copy report Published; every other repo says "not found".
			if repo == "dev" || dn[repo] {
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusPublished},
				}, nil
			}
			return nil, &catypes.ResourceNotFoundException{}
		},
		listReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			repos := make([]catypes.RepositorySummary, 0, len(allRepos))
			for _, r := range allRepos {
				repos = append(repos, catypes.RepositorySummary{Name: aws.String(r)})
			}
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: repos}, nil
		},
		deleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			if deleted != nil {
				*deleted++
			}
			return &codeartifact.DeletePackageVersionsOutput{}, nil
		},
	}
}

func TestRunRmUnfinishedDefault(t *testing.T) {
	ctx := context.Background()
	var deleted int
	cfg, _, _ := useFake(t, unfinishedCA(&deleted))
	// Default (no flags) deletes an Unfinished version with confirm bypassed.
	if err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0-rc1", false, false, true); err != nil {
		t.Fatalf("rm Unfinished: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteVersion called %d times, want 1", deleted)
	}
}

func TestRunRmPublishedRefusedWithoutForce(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, publishedCA(nil, []string{"dev"}, nil))
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", false, false, true)
	wantExit(t, err, cob.ExitConflict)
}

func TestRunRmRefusesAtLatest(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runRm(ctx, cfg, "acme/dev/tools/app@latest", true, false, true)
	wantExit(t, err, cob.ExitError)
}

func TestRunRmRequiresVersion(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	// No @version segment — partial coords.
	err := runRm(ctx, cfg, "acme/dev/tools/app", true, false, true)
	wantExit(t, err, cob.ExitError)
}

func TestRunRmEverywhereWithoutForce(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", false, true, true)
	wantExit(t, err, cob.ExitError)
}

func TestRunRmForceRefusedWithDownstream(t *testing.T) {
	ctx := context.Background()
	var deleted int
	// Source repo "dev" + downstream copies in "staging" and "prod".
	cfg, _, stderr := useFake(t, publishedCA(&deleted,
		[]string{"dev", "staging", "prod"}, []string{"staging", "prod"}))
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", true, false, true)
	wantExit(t, err, cob.ExitConflict)
	if deleted != 0 {
		t.Errorf("DeleteVersion should not run when downstream copies block: ran %d times", deleted)
	}
	// Message should name the offending repos so the operator knows where
	// to look — a bare "downstream copies exist" wouldn't actionable.
	se := stderr.String()
	if !strings.Contains(se, "staging") || !strings.Contains(se, "prod") {
		t.Errorf("error message should list downstream repos (staging, prod); got: %s", se)
	}
}

func TestRunRmForceEverywhereDeletesAnyway(t *testing.T) {
	ctx := context.Background()
	var deleted int
	cfg, _, _ := useFake(t, publishedCA(&deleted,
		[]string{"dev", "staging", "prod"}, []string{"staging", "prod"}))
	if err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", true, true, true); err != nil {
		t.Fatalf("--force --everywhere should proceed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteVersion called %d times, want 1", deleted)
	}
}

func TestRunRmForceDeletesLonePublished(t *testing.T) {
	// A Published version with no downstream copies passes the Tier 2 gate
	// without needing --everywhere — the safety net is only for actual
	// downstream copies.
	ctx := context.Background()
	var deleted int
	cfg, _, _ := useFake(t, publishedCA(&deleted, []string{"dev", "staging"}, nil))
	if err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", true, false, true); err != nil {
		t.Fatalf("--force on a lone Published version should proceed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteVersion called %d times, want 1", deleted)
	}
}

func TestRunRmMissingVersion(t *testing.T) {
	// The version genuinely doesn't exist (no Describe response). The
	// not-found path is distinct from the immutability gate.
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{
		describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		},
	})
	err := runRm(ctx, cfg, "acme/dev/tools/app@9.9.9", false, false, true)
	wantExit(t, err, cob.ExitNotFound)
}
