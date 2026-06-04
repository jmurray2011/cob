package pull

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// pullFake returns a FakeCA serving one asset (no advertised hash, so the
// pull writes without a SHA cross-check) for a single-file pull.
func pullFake() *clitest.FakeCA {
	return &clitest.FakeCA{
		ListAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			return &codeartifact.ListPackageVersionAssetsOutput{Assets: []catypes.AssetSummary{
				{Name: aws.String("app.bin"), Size: aws.Int64(5)},
			}}, nil
		},
		GetAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader("hello"))}, nil
		},
	}
}

func TestPullVerboseTrace(t *testing.T) {
	cfg, _, stderr := clitest.UseFake(t, pullFake())
	cfg.Verbose = true
	dest := filepath.Join(t.TempDir(), "out.bin") // single-file pull: no manifest write
	if err := Run(context.Background(), cfg, "acme/dev/tools/app@1.0.0", "", dest, 1); err != nil {
		t.Fatalf("pull: %v", err)
	}
	se := stderr.String()
	for _, want := range []string{
		"verbose: target acme/dev/tools/app@1.0.0", // coordinate resolution
		"verbose: app.bin",                         // per-asset method + timing
	} {
		if !strings.Contains(se, want) {
			t.Errorf("verbose stderr missing %q; got:\n%s", want, se)
		}
	}
}

func TestPullNoVerboseByDefault(t *testing.T) {
	cfg, _, stderr := clitest.UseFake(t, pullFake()) // Verbose defaults false
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := Run(context.Background(), cfg, "acme/dev/tools/app@1.0.0", "", dest, 1); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if se := stderr.String(); strings.Contains(se, "verbose:") {
		t.Errorf("no --verbose should emit no 'verbose:' lines; got:\n%s", se)
	}
}
