package promote

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

func TestGatePromote(t *testing.T) {
	ctx := context.Background()
	dest := &cob.PackageCoordinates{Domain: "d", Repository: "prod", Namespace: "n", Package: "p", Version: "1.0.0"}
	reg := func(ca *clitest.FakeCA) *cob.Registry { return cob.NewRegistry(&cob.Client{CodeArtifact: ca}) }

	t.Run("fresh destination: no gate fires", func(t *testing.T) {
		present, code, err := gatePromote(ctx, reg(&clitest.FakeCA{}), dest, "prod", "", false, false, false)
		if err != nil || code != cob.ExitOK || present != nil {
			t.Fatalf("got present=%v code=%d err=%v", present, code, err)
		}
	})
	t.Run("existing dest without --force is a conflict", func(t *testing.T) {
		_, code, err := gatePromote(ctx, reg(&clitest.FakeCA{}), dest, "prod", "Published", true, false, false)
		if code != cob.ExitConflict || err == nil {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("existing dest with --force is allowed", func(t *testing.T) {
		_, code, err := gatePromote(ctx, reg(&clitest.FakeCA{}), dest, "prod", "Published", true, false, true)
		if err != nil || code != cob.ExitOK {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("--resume with no dest errors", func(t *testing.T) {
		_, code, err := gatePromote(ctx, reg(&clitest.FakeCA{}), dest, "prod", "", false, true, false)
		if code != cob.ExitError || err == nil {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("--resume on Published (not Unfinished) errors", func(t *testing.T) {
		_, code, err := gatePromote(ctx, reg(&clitest.FakeCA{}), dest, "prod", "Published", true, true, false)
		if code != cob.ExitError || err == nil {
			t.Fatalf("got code=%d err=%v", code, err)
		}
	})
	t.Run("--resume on Unfinished returns the present map", func(t *testing.T) {
		present, code, err := gatePromote(ctx, reg(&clitest.FakeCA{ListAssetsFn: oneAsset("app.bin", 9)}),
			dest, "prod", "Unfinished", true, true, false)
		if err != nil || code != cob.ExitOK {
			t.Fatalf("got code=%d err=%v", code, err)
		}
		if _, ok := present["app.bin"]; !ok {
			t.Errorf("present = %v, want app.bin", present)
		}
	})
}

// oneAsset — see comment in publish/gate_test.go.
func oneAsset(name string, size int64) func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		return &codeartifact.ListPackageVersionAssetsOutput{
			Assets: []catypes.AssetSummary{{Name: aws.String(name), Size: aws.Int64(size)}},
		}, nil
	}
}
