package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/pkg/cob"
)

func TestRunResolve(t *testing.T) {
	ctx := context.Background()

	t.Run("partial coordinates rejected", func(t *testing.T) {
		useFake(t, &fakeCA{})
		wantExit(t, runResolve(ctx, "dom/repo"), cob.ExitError)
	})

	t.Run("no published versions -> not found", func(t *testing.T) {
		useFake(t, &fakeCA{}) // ListPackageVersions default: empty
		wantExit(t, runResolve(ctx, "dom/repo/ns/pkg"), cob.ExitNotFound)
	})

	t.Run("success", func(t *testing.T) {
		ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("2.1.0")}},
			}, nil
		}}
		useFake(t, ca)
		if err := runResolve(ctx, "dom/repo/ns/pkg"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
}

func TestRunLs(t *testing.T) {
	ctx := context.Background()

	t.Run("lists domains", func(t *testing.T) {
		ca := &fakeCA{listDomainsFn: func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error) {
			return &codeartifact.ListDomainsOutput{Domains: []catypes.DomainSummary{
				{Name: aws.String("acme"), Status: catypes.DomainStatusActive},
			}}, nil
		}}
		stdout, _ := useFake(t, ca)
		if err := runLs(ctx, "", false); err != nil {
			t.Fatalf("ls: %v", err)
		}
		if !strings.Contains(stdout.String(), "acme") {
			t.Errorf("domain listing missing 'acme': %q", stdout.String())
		}
	})

	t.Run("no domains -> not found", func(t *testing.T) {
		useFake(t, &fakeCA{})
		wantExit(t, runLs(ctx, "", false), cob.ExitNotFound)
	})

	t.Run("no assets -> not found", func(t *testing.T) {
		useFake(t, &fakeCA{}) // ListPackageVersionAssets default: empty
		wantExit(t, runLs(ctx, "dom/repo/ns/pkg@1.0.0", false), cob.ExitNotFound)
	})
}

func TestRunPull(t *testing.T) {
	ctx := context.Background()

	t.Run("version not found", func(t *testing.T) {
		ca := &fakeCA{listAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		useFake(t, ca)
		err := runPull(ctx, "dom/repo/ns/pkg@1.0.0", "", t.TempDir(), "", "", 4)
		wantExit(t, err, cob.ExitNotFound)
	})

	t.Run("requested asset not in version", func(t *testing.T) {
		ca := &fakeCA{listAssetsFn: oneAsset("real.bin", 3)}
		useFake(t, ca)
		err := runPull(ctx, "dom/repo/ns/pkg@1.0.0", "missing.bin", t.TempDir(), "", "missing.bin", 4)
		wantExit(t, err, cob.ExitNotFound)
	})

	t.Run("single asset downloaded", func(t *testing.T) {
		ca := &fakeCA{
			listAssetsFn: oneAsset("a.bin", 5),
			getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
				return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader("hello"))}, nil
			},
		}
		useFake(t, ca)
		dst := filepath.Join(t.TempDir(), "out.bin")
		if err := runPull(ctx, "dom/repo/ns/pkg@1.0.0", "a.bin", dst, "", "a.bin", 4); err != nil {
			t.Fatalf("pull: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "hello" {
			t.Fatalf("pulled file = %q, %v", got, err)
		}
	})
}

func TestRunPublish(t *testing.T) {
	ctx := context.Background()

	t.Run("conflict when version already exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the version as existing.
		useFake(t, &fakeCA{})
		mf := writeManifest(t)
		err := runPublish(ctx, mf, "1.0.0", false, false, true, 4)
		wantExit(t, err, cob.ExitConflict)
	})

	t.Run("success publishes assets plus provenance", func(t *testing.T) {
		var published int
		ca := &fakeCA{
			describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return nil, &catypes.ResourceNotFoundException{} // version does not exist yet
			},
			publishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		useFake(t, ca)
		mf := writeManifest(t)
		if err := runPublish(ctx, mf, "1.0.0", false, false, true, 4); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// one real asset + the cob-provenance.json finalizer
		if published != 2 {
			t.Errorf("PublishPackageVersion called %d times, want 2 (asset + provenance)", published)
		}
	})
}

func TestRunPromote(t *testing.T) {
	ctx := context.Background()

	t.Run("conflict when destination version exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the dest version as existing.
		useFake(t, &fakeCA{})
		err := runPromote(ctx, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, 4)
		wantExit(t, err, cob.ExitConflict)
	})

	t.Run("@latest with no source versions -> not found", func(t *testing.T) {
		useFake(t, &fakeCA{}) // ListPackageVersions default: empty
		err := runPromote(ctx, "dom/dev/ns/pkg@latest", "", "prod", false, true, 4)
		wantExit(t, err, cob.ExitNotFound)
	})
}

func TestRunValidate(t *testing.T) {
	t.Run("valid manifest", func(t *testing.T) {
		useFake(t, &fakeCA{})
		dir := t.TempDir()
		writeFile(t, dir, "x.txt", "data")
		mf := writeFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x.txt\n")
		if err := runValidate(mf, "1.0.0"); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	t.Run("duplicate basename rejected", func(t *testing.T) {
		useFake(t, &fakeCA{})
		dir := t.TempDir()
		writeFile(t, dir, "x.txt", "data")
		mf := writeFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x.txt\n  b: x.txt\n")
		wantExit(t, runValidate(mf, "1.0.0"), cob.ExitError)
	})
}

// oneAsset returns a listAssetsFn that reports a single named asset with the
// given size and no recorded hash.
func oneAsset(name string, size int64) func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		return &codeartifact.ListPackageVersionAssetsOutput{
			Assets: []catypes.AssetSummary{{Name: aws.String(name), Size: aws.Int64(size)}},
		}, nil
	}
}

// writeManifest writes a minimal one-source manifest (with its local asset
// file) to a fresh temp dir and returns the manifest path.
func writeManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "payload.txt", "payload-bytes")
	return writeFile(t, dir, "m.yaml",
		"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./payload.txt\n")
}
