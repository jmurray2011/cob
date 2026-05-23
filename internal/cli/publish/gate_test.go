package publish

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

func TestGatePublish(t *testing.T) {
	ctx := context.Background()
	coords := &cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1.0.0"}
	reg := func(ca *clitest.FakeCA) *cob.Registry { return cob.NewRegistry(&cob.Client{CodeArtifact: ca}) }

	t.Run("fresh version: no gate fires", func(t *testing.T) {
		present, code, err := gatePublish(ctx, reg(&clitest.FakeCA{}), coords, "", false, false, false)
		if err != nil || code != cob.ExitOK || present != nil {
			t.Fatalf("got present=%v code=%d err=%v, want nil/0/nil", present, code, err)
		}
	})
	t.Run("existing version without --force is a conflict", func(t *testing.T) {
		_, code, err := gatePublish(ctx, reg(&clitest.FakeCA{}), coords, "Published", true, false, false)
		if code != cob.ExitConflict || err == nil {
			t.Fatalf("got code=%d err=%v, want ExitConflict + error", code, err)
		}
	})
	t.Run("existing version with --force is allowed (caller deletes)", func(t *testing.T) {
		_, code, err := gatePublish(ctx, reg(&clitest.FakeCA{}), coords, "Published", true, false, true)
		if err != nil || code != cob.ExitOK {
			t.Fatalf("got code=%d err=%v, want ok", code, err)
		}
	})
	t.Run("--resume on missing version errors", func(t *testing.T) {
		_, code, err := gatePublish(ctx, reg(&clitest.FakeCA{}), coords, "", false, true, false)
		if code != cob.ExitError || err == nil {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("--resume on Published (not Unfinished) errors", func(t *testing.T) {
		_, code, err := gatePublish(ctx, reg(&clitest.FakeCA{}), coords, "Published", true, true, false)
		if code != cob.ExitError || err == nil {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("--resume on Unfinished returns the present map", func(t *testing.T) {
		present, code, err := gatePublish(ctx, reg(&clitest.FakeCA{ListAssetsFn: oneAsset("a.bin", 5)}),
			coords, "Unfinished", true, true, false)
		if err != nil || code != cob.ExitOK {
			t.Fatalf("got code=%d err=%v", code, err)
		}
		if _, ok := present["a.bin"]; !ok {
			t.Errorf("present = %v, want a.bin", present)
		}
	})
}

// oneAsset returns a ListAssetsFn that reports a single named asset with the
// given size and no recorded hash. Local mirror of the helper in cli/run_test.go;
// each command sub-package carries its own to stay self-contained.
func oneAsset(name string, size int64) func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		return &codeartifact.ListPackageVersionAssetsOutput{
			Assets: []catypes.AssetSummary{{Name: aws.String(name), Size: aws.Int64(size)}},
		}, nil
	}
}
