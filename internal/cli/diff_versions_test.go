package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// versionedAssets returns a ListAssetsFn that picks the asset list by the
// version (and optionally repository) in the request. Lets one clitest.FakeCA
// serve both sides of a diff with different fixtures.
func versionedAssets(byVersion map[string][]catypes.AssetSummary) func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return func(in *codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		v := aws.ToString(in.PackageVersion)
		if a, ok := byVersion[v]; ok {
			return &codeartifact.ListPackageVersionAssetsOutput{Assets: a}, nil
		}
		return nil, &catypes.ResourceNotFoundException{}
	}
}

// hashedAsset is a small shorthand for an AssetSummary with a SHA-256.
func hashedAsset(name string, size int64, sha string) catypes.AssetSummary {
	return catypes.AssetSummary{
		Name:   aws.String(name),
		Size:   aws.Int64(size),
		Hashes: map[string]string{"SHA-256": sha},
	}
}

func TestRunDiffVersionsIdentical(t *testing.T) {
	ctx := context.Background()
	// Same assets on both sides — exit 0, "0 changed".
	ca := &clitest.FakeCA{ListAssetsFn: versionedAssets(map[string][]catypes.AssetSummary{
		"2.0.0": {hashedAsset("app.bin", 10, "aa")},
		"2.1.0": {hashedAsset("app.bin", 10, "aa")},
	})}
	cfg, stdout, _ := clitest.UseFake(t, ca)
	if err := runDiff(ctx, cfg, []string{"d/r/n/p@2.0.0", "d/r/n/p@2.1.0"}, "", false, false, false); err != nil {
		t.Fatalf("diff identical: %v", err)
	}
	if !strings.Contains(stdout.String(), "0 changed") {
		t.Errorf("expected '0 changed' in summary, got:\n%s", stdout.String())
	}
}

func TestRunDiffVersionsAddedRemovedChanged(t *testing.T) {
	ctx := context.Background()
	// Left has app.bin (same on both sides) and old.bin (removed in right).
	// Right has app.bin (same), new.bin (added), and config.json with a
	// different hash (changed).
	ca := &clitest.FakeCA{ListAssetsFn: versionedAssets(map[string][]catypes.AssetSummary{
		"2.0.0": {
			hashedAsset("app.bin", 10, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			hashedAsset("config.json", 5, "1111111111111111111111111111111111111111111111111111111111111111"),
			hashedAsset("old.bin", 7, "deadbeef00000000000000000000000000000000000000000000000000000000"),
		},
		"2.1.0": {
			hashedAsset("app.bin", 10, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			hashedAsset("config.json", 5, "2222222222222222222222222222222222222222222222222222222222222222"),
			hashedAsset("new.bin", 9, "feedface00000000000000000000000000000000000000000000000000000000"),
		},
	})}
	cfg, stdout, _ := clitest.UseFake(t, ca)
	err := runDiff(ctx, cfg, []string{"d/r/n/p@2.0.0", "d/r/n/p@2.1.0"}, "", false, false, false)
	clitest.WantExit(t, err, cob.ExitMismatch) // drift = added+removed+changed > 0

	out := stdout.String()
	// One per change kind plus the summary line.
	for _, want := range []string{
		"+ new.bin",
		"- old.bin",
		"~ config.json",
		"1 added, 1 removed, 1 changed, 1 same",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRunDiffVersionsIgnoresProvenanceAsset(t *testing.T) {
	// cob-provenance.json on both sides should be ignored — its bytes
	// always differ between versions (chain timestamps, IDs) but that's
	// not a package change. The diff must come out clean.
	ctx := context.Background()
	ca := &clitest.FakeCA{ListAssetsFn: versionedAssets(map[string][]catypes.AssetSummary{
		"2.0.0": {
			hashedAsset("app.bin", 10, "aa"),
			hashedAsset(cob.ProvenanceFile, 99, "leftprov"),
		},
		"2.1.0": {
			hashedAsset("app.bin", 10, "aa"),
			hashedAsset(cob.ProvenanceFile, 99, "rightprov"),
		},
	})}
	cfg, _, _ := clitest.UseFake(t, ca)
	if err := runDiff(ctx, cfg, []string{"d/r/n/p@2.0.0", "d/r/n/p@2.1.0"}, "", false, false, false); err != nil {
		t.Fatalf("diff ignoring provenance: %v", err)
	}
}

func TestRunDiffVersionsRejectsCrossPackage(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
	err := runDiff(ctx, cfg, []string{"d/r/ns1/a@1.0.0", "d/r/ns2/b@1.0.0"}, "", false, false, false)
	clitest.WantExit(t, err, cob.ExitError)
}

func TestRunDiffVersionsRequiresVersions(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
	err := runDiff(ctx, cfg, []string{"d/r/n/p", "d/r/n/p@1.0.0"}, "", false, false, false)
	clitest.WantExit(t, err, cob.ExitError)
}

func TestRunDiffVersionsCrossRepoSamePackage(t *testing.T) {
	// Cross-repo same package — the natural way to ask "did promote
	// preserve the bytes?". Same SHA on both sides → clean diff.
	ctx := context.Background()
	ca := &clitest.FakeCA{ListAssetsFn: versionedAssets(map[string][]catypes.AssetSummary{
		"2.1.0": {hashedAsset("app.bin", 10, "aa")},
	})}
	cfg, _, _ := clitest.UseFake(t, ca)
	if err := runDiff(ctx, cfg, []string{"d/dev/n/p@2.1.0", "d/prod/n/p@2.1.0"}, "", false, false, false); err != nil {
		t.Fatalf("cross-repo diff: %v", err)
	}
}
