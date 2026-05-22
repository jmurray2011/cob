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

	"github.com/jmurray2011/cob/internal/cob"
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

	t.Run("success prints the resolved version", func(t *testing.T) {
		ca := &fakeCA{listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("2.1.0")}},
			}, nil
		}}
		stdout, _ := useFake(t, ca)
		if err := runResolve(ctx, "dom/repo/ns/pkg"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "2.1.0" {
			t.Errorf("resolve printed %q, want 2.1.0", got)
		}
	})
}

func TestRunManifest(t *testing.T) {
	ca := &fakeCA{
		getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return nil, &catypes.ResourceNotFoundException{} // no cob-provenance.json
		},
		listAssetsFn: oneAsset("app.bin", 7),
	}
	stdout, _ := useFake(t, ca)
	if err := runManifest(context.Background(), "dom/repo/ns/pkg@1.0.0", ""); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	got := stdout.String()
	for _, want := range []string{
		"domain: dom", "repository: repo", "package: pkg", "sources:",
		"app.bin: ca://dom/repo/ns/pkg@1.0.0/app.bin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest output missing %q:\n%s", want, got)
		}
	}
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

	t.Run("--json not-found emits an empty array, not an object", func(t *testing.T) {
		stdout, _ := useFake(t, &fakeCA{})
		flagJSON = true // useFake restores it on cleanup
		wantExit(t, runLs(ctx, "dom/repo", false), cob.ExitNotFound)
		if got := strings.TrimSpace(stdout.String()); got != "[]" {
			t.Errorf("ls --json not-found stdout = %q, want []", got)
		}
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
		err := runPublish(ctx, mf, "1.0.0", false, false, true, false, 4)
		wantExit(t, err, cob.ExitConflict)
	})

	t.Run("dry-run works even when the version already exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the version as existing; a
		// dry run must still preview rather than exit with a conflict.
		useFake(t, &fakeCA{})
		mf := writeManifest(t)
		if err := runPublish(ctx, mf, "1.0.0", false, true, true, false, 4); err != nil {
			t.Fatalf("dry-run with existing version must not error, got %v", err)
		}
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
		if err := runPublish(ctx, mf, "1.0.0", false, false, true, false, 4); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// one real asset + the cob-provenance.json finalizer
		if published != 2 {
			t.Errorf("PublishPackageVersion called %d times, want 2 (asset + provenance)", published)
		}
	})

	t.Run("resume uploads only the missing assets", func(t *testing.T) {
		var published int
		ca := &fakeCA{
			describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
				}, nil
			},
			listAssetsFn: oneAsset("payload.txt", 13), // the manifest's lone asset is already present
			publishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		useFake(t, ca)
		mf := writeManifest(t)
		if err := runPublish(ctx, mf, "1.0.0", false, false, true, true, 4); err != nil {
			t.Fatalf("resume: %v", err)
		}
		// payload.txt is already present -> skipped; only cob-provenance.json uploads.
		if published != 1 {
			t.Errorf("resume published %d times, want 1 (just the provenance finalizer)", published)
		}
	})

	t.Run("resume with no unfinished version is an error", func(t *testing.T) {
		ca := &fakeCA{describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		useFake(t, ca)
		wantExit(t, runPublish(ctx, writeManifest(t), "1.0.0", false, false, true, true, 4), cob.ExitError)
	})

	t.Run("resume and force are mutually exclusive", func(t *testing.T) {
		useFake(t, &fakeCA{})
		wantExit(t, runPublish(ctx, writeManifest(t), "1.0.0", true, false, true, true, 4), cob.ExitError)
	})
}

func TestRunPromote(t *testing.T) {
	ctx := context.Background()

	t.Run("conflict when destination version exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the dest version as existing.
		useFake(t, &fakeCA{})
		err := runPromote(ctx, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, false, 4)
		wantExit(t, err, cob.ExitConflict)
	})

	t.Run("@latest with no source versions -> not found", func(t *testing.T) {
		useFake(t, &fakeCA{}) // ListPackageVersions default: empty
		err := runPromote(ctx, "dom/dev/ns/pkg@latest", "", "prod", false, true, false, 4)
		wantExit(t, err, cob.ExitNotFound)
	})

	t.Run("dry-run works even when the destination version exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the dest version as
		// existing; --dry-run must preview, not exit with a conflict.
		ca := &fakeCA{listAssetsFn: oneAsset("app.bin", 9)}
		stdout, _ := useFake(t, ca)
		if err := runPromote(ctx, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, true, 4); err != nil {
			t.Fatalf("dry-run with existing dest must not error, got %v", err)
		}
		if !strings.Contains(stdout.String(), "app.bin") {
			t.Errorf("dry-run should still list the asset: %q", stdout.String())
		}
	})

	t.Run("dry-run lists assets and promotes nothing", func(t *testing.T) {
		var published int
		ca := &fakeCA{
			describeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return nil, &catypes.ResourceNotFoundException{} // dest version absent
			},
			listAssetsFn: oneAsset("app.bin", 9),
			publishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		stdout, _ := useFake(t, ca)
		if err := runPromote(ctx, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, true, 4); err != nil {
			t.Fatalf("dry-run: %v", err)
		}
		if published != 0 {
			t.Errorf("dry-run published %d assets, want 0", published)
		}
		if !strings.Contains(stdout.String(), "app.bin") {
			t.Errorf("dry-run output should list the asset: %q", stdout.String())
		}
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

	t.Run("reserved asset name cob-provenance.json rejected", func(t *testing.T) {
		useFake(t, &fakeCA{})
		dir := t.TempDir()
		writeFile(t, dir, "cob-provenance.json", "{}")
		mf := writeFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  shadow: ./cob-provenance.json\n")
		wantExit(t, runValidate(mf, "1.0.0"), cob.ExitError)
	})

	t.Run("duplicate basename rejected", func(t *testing.T) {
		useFake(t, &fakeCA{})
		dir := t.TempDir()
		writeFile(t, dir, "x.txt", "data")
		mf := writeFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x.txt\n  b: x.txt\n")
		wantExit(t, runValidate(mf, "1.0.0"), cob.ExitError)
	})
}

func TestSelectAssets(t *testing.T) {
	all := []cob.AssetInfo{{Name: "a.bin"}, {Name: "b.bin"}, {Name: "c.bin"}}

	t.Run("no filter returns everything", func(t *testing.T) {
		got, un, err := selectAssets(all, "", "")
		if err != nil || len(got) != 3 || len(un) != 0 {
			t.Fatalf("got %d assets, unmatched %v, err %v", len(got), un, err)
		}
	})
	t.Run("positional asset", func(t *testing.T) {
		got, _, err := selectAssets(all, "b.bin", "")
		if err != nil || len(got) != 1 || got[0].Name != "b.bin" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})
	t.Run("positional miss is an error", func(t *testing.T) {
		if _, _, err := selectAssets(all, "nope.bin", ""); err == nil {
			t.Fatal("expected a not-found error")
		}
	})
	t.Run("comma filter, trims spaces, reports a miss", func(t *testing.T) {
		got, un, err := selectAssets(all, "", "a.bin, nope.bin ,c.bin")
		if err != nil {
			t.Fatalf("err %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d assets, want 2 (a.bin, c.bin)", len(got))
		}
		if len(un) != 1 || un[0] != "nope.bin" {
			t.Errorf("unmatched = %v, want [nope.bin]", un)
		}
	})
	t.Run("comma filter matching nothing is an error", func(t *testing.T) {
		if _, _, err := selectAssets(all, "", "x,y"); err == nil {
			t.Fatal("expected an error when nothing matched")
		}
	})
}

func TestReconcilePromotedAssets(t *testing.T) {
	recorded := []cob.ProvenanceEntry{
		{Asset: "app.bin", Key: "app", Source: "s3://b/app", SHA256: "old", Origin: &cob.Origin{Type: "s3"}},
		{Asset: "gone.bin", Key: "gone"}, // recorded but no longer present
	}
	results := []*cob.AssetResult{
		{Name: "app.bin", SHA256: "new", Size: 10},
		{Name: "extra.bin", SHA256: "x", Size: 5}, // promoted, never recorded
	}
	got := reconcilePromotedAssets(recorded, []string{"app.bin", "extra.bin"}, results)

	if len(got) != 2 {
		t.Fatalf("got %d entries, want exactly the 2 promoted assets", len(got))
	}
	// app.bin: hash/size from the actual promote, Origin/Source from the record.
	if got[0].Asset != "app.bin" || got[0].SHA256 != "new" || got[0].Size != 10 {
		t.Errorf("app.bin not rebuilt from results: %+v", got[0])
	}
	if got[0].Source != "s3://b/app" || got[0].Origin == nil {
		t.Errorf("app.bin lost recorded Origin/Source: %+v", got[0])
	}
	// extra.bin: added out-of-band, still gets an honest entry.
	if got[1].Asset != "extra.bin" || got[1].SHA256 != "x" {
		t.Errorf("extra.bin not recorded: %+v", got[1])
	}
	// gone.bin: recorded but not promoted -> must not appear.
	for _, e := range got {
		if e.Asset == "gone.bin" {
			t.Error("gone.bin was not promoted; it must not appear in provenance")
		}
	}
}

// helloSHA is sha256("hello") — used by the verify/diff tests against a
// manifest whose local source is the literal "hello".
const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// publishedAsset returns a listAssetsFn that reports a single named asset
// with the given size and SHA-256 (e.g. what compareManifestToPublished
// reads back from CodeArtifact).
func publishedAsset(name string, size int64, sha string) func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
		return &codeartifact.ListPackageVersionAssetsOutput{
			Assets: []catypes.AssetSummary{{
				Name:   aws.String(name),
				Size:   aws.Int64(size),
				Hashes: map[string]string{"SHA-256": sha},
			}},
		}, nil
	}
}

// writeHelloManifest writes a one-source manifest whose asset is the local
// file "hello" (SHA-256 = helloSHA).
func writeHelloManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "payload.txt", "hello")
	return writeFile(t, dir, "m.yaml",
		"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./payload.txt\n")
}

func TestRunVerify(t *testing.T) {
	ctx := context.Background()

	t.Run("clean match -> exit 0", func(t *testing.T) {
		useFake(t, &fakeCA{listAssetsFn: publishedAsset("payload.txt", 5, helloSHA)})
		if err := runVerify(ctx, writeHelloManifest(t), "1.0.0", true); err != nil {
			t.Fatalf("verify clean: %v", err)
		}
	})

	t.Run("SHA mismatch -> exit 4 (ExitMismatch)", func(t *testing.T) {
		useFake(t, &fakeCA{listAssetsFn: publishedAsset("payload.txt", 5,
			"deadbeef0000000000000000000000000000000000000000000000000000beef")})
		wantExit(t, runVerify(ctx, writeHelloManifest(t), "1.0.0", true), cob.ExitMismatch)
	})

	t.Run("source resolution failure -> exit 1 (ExitError)", func(t *testing.T) {
		// publisher claims payload.txt exists, but the local source is missing
		// -> per-asset Resolve fails -> opErrors > 0 -> the check didn't run.
		useFake(t, &fakeCA{listAssetsFn: publishedAsset("missing.txt", 5, helloSHA)})
		dir := t.TempDir()
		mf := writeFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./missing.txt\n")
		wantExit(t, runVerify(ctx, mf, "1.0.0", true), cob.ExitError)
	})

	t.Run("coords @latest with no published versions -> exit 2", func(t *testing.T) {
		useFake(t, &fakeCA{}) // ListPackageVersions default: empty
		wantExit(t, runVerify(ctx, "dom/repo/ns/pkg", "latest", false), cob.ExitNotFound)
	})
}

func TestRunDiff(t *testing.T) {
	ctx := context.Background()

	t.Run("clean -> exit 0", func(t *testing.T) {
		useFake(t, &fakeCA{listAssetsFn: publishedAsset("payload.txt", 5, helloSHA)})
		if err := runDiff(ctx, writeHelloManifest(t), "1.0.0", true); err != nil {
			t.Fatalf("diff clean: %v", err)
		}
	})

	t.Run("drift -> exit 4 (ExitMismatch)", func(t *testing.T) {
		useFake(t, &fakeCA{listAssetsFn: publishedAsset("payload.txt", 5,
			"deadbeef0000000000000000000000000000000000000000000000000000beef")})
		wantExit(t, runDiff(ctx, writeHelloManifest(t), "1.0.0", true), cob.ExitMismatch)
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
