package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// unfinishedCA returns a clitest.FakeCA whose DescribePackageVersion reports the
// version as Unfinished — the cleanup case that rm allows by default.
func unfinishedCA(deleted *int) *clitest.FakeCA {
	return &clitest.FakeCA{
		DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
			}, nil
		},
		DeleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			if deleted != nil {
				*deleted++
			}
			return &codeartifact.DeletePackageVersionsOutput{}, nil
		},
	}
}

// publishedCA returns a clitest.FakeCA whose DescribePackageVersion reports the
// version as Published (a real release). Used to verify the Tier 1 gate.
// downstreamRepos, if non-nil, makes the version exist in *those* repos
// too (matched against input.Repository) — every other repo gets a
// ResourceNotFound. ListRepositoriesInDomain returns every name in
// allRepos.
func publishedCA(deleted *int, allRepos, downstreamRepos []string) *clitest.FakeCA {
	dn := make(map[string]bool, len(downstreamRepos))
	for _, r := range downstreamRepos {
		dn[r] = true
	}
	return &clitest.FakeCA{
		DescribeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
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
		ListReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			repos := make([]catypes.RepositorySummary, 0, len(allRepos))
			for _, r := range allRepos {
				repos = append(repos, catypes.RepositorySummary{Name: aws.String(r)})
			}
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: repos}, nil
		},
		DeleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
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
	cfg, _, _ := clitest.UseFake(t, unfinishedCA(&deleted))
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
	cfg, _, _ := clitest.UseFake(t, publishedCA(nil, []string{"dev"}, nil))
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", false, false, true)
	clitest.WantExit(t, err, cob.ExitConflict)
}

func TestRunRmRefusesAtLatest(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
	err := runRm(ctx, cfg, "acme/dev/tools/app@latest", true, false, true)
	clitest.WantExit(t, err, cob.ExitError)
}

func TestRunRmRequiresVersion(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
	// No @version segment — partial coords.
	err := runRm(ctx, cfg, "acme/dev/tools/app", true, false, true)
	clitest.WantExit(t, err, cob.ExitError)
}

func TestRunRmEverywhereWithoutForce(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", false, true, true)
	clitest.WantExit(t, err, cob.ExitError)
}

func TestRunRmForceRefusedWithDownstream(t *testing.T) {
	ctx := context.Background()
	var deleted int
	// Source repo "dev" + downstream copies in "staging" and "prod".
	cfg, _, stderr := clitest.UseFake(t, publishedCA(&deleted,
		[]string{"dev", "staging", "prod"}, []string{"staging", "prod"}))
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", true, false, true)
	clitest.WantExit(t, err, cob.ExitConflict)
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

func TestRunRmForceRefusedWhenDownstreamProbeErrors(t *testing.T) {
	ctx := context.Background()
	var deleted int
	// "dev" holds the Published version being deleted. The downstream probe
	// to "staging" fails transiently (throttle / creds blip); "prod" has no
	// copy. A failed probe must NOT be read as "no copy" — that would let
	// --force silently break staging's chain-of-evidence without the
	// --everywhere consent the downstream-found path already requires.
	ca := &clitest.FakeCA{
		DescribeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			switch aws.ToString(in.Repository) {
			case "dev":
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusPublished},
				}, nil
			case "staging":
				return nil, errors.New("ThrottlingException: rate exceeded")
			default: // prod and anything else
				return nil, &catypes.ResourceNotFoundException{}
			}
		},
		ListReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: []catypes.RepositorySummary{
				{Name: aws.String("dev")}, {Name: aws.String("staging")}, {Name: aws.String("prod")},
			}}, nil
		},
		DeleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			deleted++
			return &codeartifact.DeletePackageVersionsOutput{}, nil
		},
	}
	cfg, _, stderr := clitest.UseFake(t, ca)
	// --force, NOT --everywhere: an unverifiable downstream must block, not proceed.
	err := runRm(ctx, cfg, "acme/dev/tools/app@2.1.0", true, false, true)
	clitest.WantExit(t, err, cob.ExitConflict)
	if deleted != 0 {
		t.Errorf("DeleteVersion must not run when a downstream probe failed: ran %d times", deleted)
	}
	// The message should name the repo it couldn't verify so the operator
	// knows where the uncertainty is — not a bare "try again".
	if se := stderr.String(); !strings.Contains(se, "staging") {
		t.Errorf("error should name the unverifiable repo (staging); got: %s", se)
	}
}

func TestRunRmForceEverywhereDeletesAnyway(t *testing.T) {
	ctx := context.Background()
	var deleted int
	cfg, _, _ := clitest.UseFake(t, publishedCA(&deleted,
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
	cfg, _, _ := clitest.UseFake(t, publishedCA(&deleted, []string{"dev", "staging"}, nil))
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
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{
		DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		},
	})
	err := runRm(ctx, cfg, "acme/dev/tools/app@9.9.9", false, false, true)
	clitest.WantExit(t, err, cob.ExitNotFound)
}
