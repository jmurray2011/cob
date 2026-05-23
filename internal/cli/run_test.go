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

	"github.com/jmurray2011/cob/internal/cli/diff"
	"github.com/jmurray2011/cob/internal/cli/promote"
	"github.com/jmurray2011/cob/internal/cli/publish"
	"github.com/jmurray2011/cob/internal/cli/pull"
	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

func TestRunResolve(t *testing.T) {
	ctx := context.Background()

	t.Run("partial coordinates rejected", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		clitest.WantExit(t, runResolve(ctx, cfg, []string{"dom/repo"}), cob.ExitError)
	})

	t.Run("no published versions -> not found", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{}) // ListPackageVersions default: empty
		clitest.WantExit(t, runResolve(ctx, cfg, []string{"dom/repo/ns/pkg"}), cob.ExitNotFound)
	})

	t.Run("success prints the resolved version", func(t *testing.T) {
		ca := &clitest.FakeCA{ListVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("2.1.0")}},
			}, nil
		}}
		cfg, stdout, _ := clitest.UseFake(t, ca)
		if err := runResolve(ctx, cfg, []string{"dom/repo/ns/pkg"}); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "2.1.0" {
			t.Errorf("resolve printed %q, want 2.1.0", got)
		}
	})
}

func TestRunManifest(t *testing.T) {
	ca := &clitest.FakeCA{
		GetAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return nil, &catypes.ResourceNotFoundException{} // no cob-provenance.json
		},
		ListAssetsFn: oneAsset("app.bin", 7),
	}
	cfg, stdout, _ := clitest.UseFake(t, ca)
	if err := runManifest(context.Background(), cfg, []string{"dom/repo/ns/pkg@1.0.0"}, ""); err != nil {
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
		ca := &clitest.FakeCA{ListDomainsFn: func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error) {
			return &codeartifact.ListDomainsOutput{Domains: []catypes.DomainSummary{
				{Name: aws.String("acme"), Status: catypes.DomainStatusActive},
			}}, nil
		}}
		cfg, stdout, _ := clitest.UseFake(t, ca)
		if err := runLs(ctx, cfg, ""); err != nil {
			t.Fatalf("ls: %v", err)
		}
		if !strings.Contains(stdout.String(), "acme") {
			t.Errorf("domain listing missing 'acme': %q", stdout.String())
		}
	})

	t.Run("no domains -> not found", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		clitest.WantExit(t, runLs(ctx, cfg, ""), cob.ExitNotFound)
	})

	t.Run("no assets -> not found", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{}) // ListPackageVersionAssets default: empty
		clitest.WantExit(t, runLs(ctx, cfg, "dom/repo/ns/pkg@1.0.0"), cob.ExitNotFound)
	})

	t.Run("--json not-found emits an empty array, not an object", func(t *testing.T) {
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{})
		cfg.JSON = true
		clitest.WantExit(t, runLs(ctx, cfg, "dom/repo"), cob.ExitNotFound)
		if got := strings.TrimSpace(stdout.String()); got != "[]" {
			t.Errorf("ls --json not-found stdout = %q, want []", got)
		}
	})
}

func TestRunPull(t *testing.T) {
	ctx := context.Background()

	t.Run("version not found", func(t *testing.T) {
		ca := &clitest.FakeCA{ListAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		cfg, _, _ := clitest.UseFake(t, ca)
		err := pull.Run(ctx, cfg, "dom/repo/ns/pkg@1.0.0", "", t.TempDir(), "", "", 4)
		clitest.WantExit(t, err, cob.ExitNotFound)
	})

	t.Run("requested asset not in version", func(t *testing.T) {
		ca := &clitest.FakeCA{ListAssetsFn: oneAsset("real.bin", 3)}
		cfg, _, _ := clitest.UseFake(t, ca)
		err := pull.Run(ctx, cfg, "dom/repo/ns/pkg@1.0.0", "missing.bin", t.TempDir(), "", "missing.bin", 4)
		clitest.WantExit(t, err, cob.ExitNotFound)
	})

	t.Run("single asset downloaded", func(t *testing.T) {
		ca := &clitest.FakeCA{
			ListAssetsFn: oneAsset("a.bin", 5),
			GetAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
				return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader("hello"))}, nil
			},
		}
		cfg, _, _ := clitest.UseFake(t, ca)
		dst := filepath.Join(t.TempDir(), "out.bin")
		if err := pull.Run(ctx, cfg, "dom/repo/ns/pkg@1.0.0", "a.bin", dst, "", "a.bin", 4); err != nil {
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
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		mf := writeManifest(t)
		err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, false, 4)
		clitest.WantExit(t, err, cob.ExitConflict)
	})

	t.Run("dry-run works even when the version already exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the version as existing; a
		// dry run must still preview rather than exit with a conflict.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		mf := writeManifest(t)
		if err := publish.Run(ctx, cfg, mf, "1.0.0", false, true, true, false, 4); err != nil {
			t.Fatalf("dry-run with existing version must not error, got %v", err)
		}
	})

	t.Run("success publishes assets plus provenance", func(t *testing.T) {
		var published int
		ca := &clitest.FakeCA{
			DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return nil, &catypes.ResourceNotFoundException{} // version does not exist yet
			},
			PublishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		cfg, _, _ := clitest.UseFake(t, ca)
		mf := writeManifest(t)
		if err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, false, 4); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// one real asset + the cob-provenance.json finalizer
		if published != 2 {
			t.Errorf("PublishPackageVersion called %d times, want 2 (asset + provenance)", published)
		}
	})

	t.Run("resume uploads only the missing assets", func(t *testing.T) {
		var published int
		ca := &clitest.FakeCA{
			DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
				}, nil
			},
			ListAssetsFn: oneAsset("payload.txt", 13), // the manifest's lone asset is already present
			PublishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		cfg, _, _ := clitest.UseFake(t, ca)
		mf := writeManifest(t)
		if err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, true, 4); err != nil {
			t.Fatalf("resume: %v", err)
		}
		// payload.txt is already present -> skipped; only cob-provenance.json uploads.
		if published != 1 {
			t.Errorf("resume published %d times, want 1 (just the provenance finalizer)", published)
		}
	})

	t.Run("resume with no unfinished version is an error", func(t *testing.T) {
		ca := &clitest.FakeCA{DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		cfg, _, _ := clitest.UseFake(t, ca)
		clitest.WantExit(t, publish.Run(ctx, cfg, writeManifest(t), "1.0.0", false, false, true, true, 4), cob.ExitError)
	})

	t.Run("resume and force are mutually exclusive", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		clitest.WantExit(t, publish.Run(ctx, cfg, writeManifest(t), "1.0.0", true, false, true, true, 4), cob.ExitError)
	})
}

func TestRunPromote(t *testing.T) {
	ctx := context.Background()

	t.Run("conflict when destination version exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the dest version as existing.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		err := promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, false, false, 4)
		clitest.WantExit(t, err, cob.ExitConflict)
	})

	t.Run("@latest with no source versions -> not found", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{}) // ListPackageVersions default: empty
		err := promote.Run(ctx, cfg, "dom/dev/ns/pkg@latest", "", "prod", false, true, false, false, 4)
		clitest.WantExit(t, err, cob.ExitNotFound)
	})

	t.Run("dry-run works even when the destination version exists", func(t *testing.T) {
		// Default DescribePackageVersion reports the dest version as
		// existing; --dry-run must preview, not exit with a conflict.
		ca := &clitest.FakeCA{ListAssetsFn: oneAsset("app.bin", 9)}
		cfg, stdout, _ := clitest.UseFake(t, ca)
		if err := promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, true, false, 4); err != nil {
			t.Fatalf("dry-run with existing dest must not error, got %v", err)
		}
		if !strings.Contains(stdout.String(), "app.bin") {
			t.Errorf("dry-run should still list the asset: %q", stdout.String())
		}
	})

	t.Run("resume copies only the missing assets", func(t *testing.T) {
		var copied int
		ca := &clitest.FakeCA{
			DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusUnfinished},
				}, nil
			},
			ListAssetsFn: oneAsset("app.bin", 9), // already in dest
			PublishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				copied++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		cfg, _, _ := clitest.UseFake(t, ca)
		if err := promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, false, true, 4); err != nil {
			t.Fatalf("resume: %v", err)
		}
		// app.bin already present -> skipped; only cob-provenance.json publishes.
		if copied != 1 {
			t.Errorf("resume copied %d times, want 1 (just the provenance finalizer)", copied)
		}
	})

	t.Run("resume with no unfinished dest version errors", func(t *testing.T) {
		ca := &clitest.FakeCA{DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		cfg, _, _ := clitest.UseFake(t, ca)
		clitest.WantExit(t, promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, false, true, 4), cob.ExitError)
	})

	t.Run("resume and force are mutually exclusive", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		clitest.WantExit(t, promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", true, true, false, true, 4), cob.ExitError)
	})

	t.Run("dry-run lists assets and promotes nothing", func(t *testing.T) {
		var published int
		ca := &clitest.FakeCA{
			DescribeFn: func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
				return nil, &catypes.ResourceNotFoundException{} // dest version absent
			},
			ListAssetsFn: oneAsset("app.bin", 9),
			PublishFn: func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				published++
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		cfg, stdout, _ := clitest.UseFake(t, ca)
		if err := promote.Run(ctx, cfg, "dom/dev/ns/pkg@1.0.0", "", "prod", false, true, true, false, 4); err != nil {
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

// TestRunDiffLint covers the offline-lint mode that replaced
// `cob validate`. Same per-source checks (schema, URI syntax, local
// existence) but exposed via `cob diff <manifest>` with no --version.
// Implicit validation in publish/pull/promote uses the same code path
// (cliutil.ValidateManifest in validation.go), so these tests double-cover the
// pre-flight guard those commands now perform.
func TestRunDiffLint(t *testing.T) {
	ctx := context.Background()

	t.Run("local manifest renders file existence + size", func(t *testing.T) {
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "x.txt", "data") // 4 bytes
		mf := clitest.WriteFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x.txt\n")
		if err := diff.Run(ctx, cfg, []string{mf}, "", false, false, false); err != nil {
			t.Fatalf("diff lint: %v", err)
		}
		out := stdout.String()
		if !strings.Contains(out, "✓ a") || !strings.Contains(out, "(4 B)") {
			t.Errorf("local file line missing or wrong:\n%s", out)
		}
		if !strings.Contains(out, "1 local files verified to exist") {
			t.Errorf("summary should distinguish local vs remote:\n%s", out)
		}
	})

	t.Run("remote-only manifest renders syntax-only and explains the gap", func(t *testing.T) {
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  app: s3://bucket/app.bin\n")
		if err := diff.Run(ctx, cfg, []string{mf}, "", false, false, false); err != nil {
			t.Fatalf("diff lint remote: %v", err)
		}
		out := stdout.String()
		if !strings.Contains(out, "(remote, syntax only)") {
			t.Errorf("remote source should be labelled syntax-only:\n%s", out)
		}
		// Summary points at the way to actually diff bytes — the
		// equivalent of the old "use `cob verify`" hint now points at
		// --version.
		if !strings.Contains(out, "--version") {
			t.Errorf("summary should mention --version for byte integrity:\n%s", out)
		}
		if strings.Contains(out, "0 B") || strings.Contains(out, "0ms") {
			t.Errorf("lint must not fabricate size/duration for remote sources:\n%s", out)
		}
	})

	t.Run("missing local file is flagged", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./missing.txt\n")
		err := diff.Run(ctx, cfg, []string{mf}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitError)
	})

	t.Run("reserved asset name cob-provenance.json rejected", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "cob-provenance.json", "{}")
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  shadow: ./cob-provenance.json\n")
		clitest.WantExit(t, diff.Run(ctx, cfg, []string{mf}, "", false, false, false), cob.ExitError)
	})

	t.Run("duplicate basename rejected", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "x.txt", "data")
		mf := clitest.WriteFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x.txt\n  b: x.txt\n")
		clitest.WantExit(t, diff.Run(ctx, cfg, []string{mf}, "", false, false, false), cob.ExitError)
	})

	t.Run("publish refuses a manifest the lint would reject", func(t *testing.T) {
		// Implicit cliutil.ValidateManifest at the top of runPublish should
		// short-circuit before any AWS work, exiting with cliutil.ExitError —
		// same code path as `cob diff <manifest>` exposes.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./never-existed.txt\n")
		err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, false, 4)
		clitest.WantExit(t, err, cob.ExitError)
	})
}

// helloSHA is sha256("hello") — used by the verify/diff tests against a
// manifest whose local source is the literal "hello".
const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// publishedAsset returns a ListAssetsFn that reports a single named asset
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
	clitest.WriteFile(t, dir, "payload.txt", "hello")
	return clitest.WriteFile(t, dir, "m.yaml",
		"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./payload.txt\n")
}

// TestRunDiff covers diff's four AWS-touching modes (the offline-lint
// fifth mode is in TestRunDiffLint above). One TestRunDiff function,
// organized by mode, so the test file mirrors how a reader thinks
// about the dispatcher: "given these positional args, which path?"
func TestRunDiff(t *testing.T) {
	ctx := context.Background()

	// --- Manifest mode (one arg + --version) ----------------------------------

	t.Run("manifest: clean match -> exit 0", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5, helloSHA)})
		if err := diff.Run(ctx, cfg, []string{writeHelloManifest(t)}, "1.0.0", true, false, false); err != nil {
			t.Fatalf("diff manifest clean: %v", err)
		}
	})

	t.Run("manifest: uppercase-hex SHA still matches (case-insensitive)", func(t *testing.T) {
		// hex SHA-256 is case-insensitive; different sources return
		// upper vs lower. A case-sensitive compare here would be a
		// spurious mismatch on bytes that are actually identical.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5, strings.ToUpper(helloSHA))})
		if err := diff.Run(ctx, cfg, []string{writeHelloManifest(t)}, "1.0.0", true, false, false); err != nil {
			t.Fatalf("uppercase-hex must still match the lowercase source hash: %v", err)
		}
	})

	t.Run("manifest: SHA mismatch -> exit 4 (ExitMismatch)", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5,
			"deadbeef0000000000000000000000000000000000000000000000000000beef")})
		clitest.WantExit(t, diff.Run(ctx, cfg, []string{writeHelloManifest(t)}, "1.0.0", true, false, false), cob.ExitMismatch)
	})

	t.Run("manifest: missing local source bails at implicit validation, not at compare", func(t *testing.T) {
		// cliutil.ValidateManifest at the top of runDiffManifest catches this
		// before any AWS work — used to be an "op error during diff"
		// path, now it's a clean pre-flight failure.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("missing.txt", 5, helloSHA)})
		dir := t.TempDir()
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./missing.txt\n")
		clitest.WantExit(t, diff.Run(ctx, cfg, []string{mf}, "1.0.0", true, false, false), cob.ExitError)
	})

	// --- Self-check mode (one arg, coords with version) -----------------------

	t.Run("self-check: @latest with no published versions -> exit 2", func(t *testing.T) {
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{}) // ListPackageVersions default: empty
		clitest.WantExit(t, diff.Run(ctx, cfg, []string{"dom/repo/ns/pkg@latest"}, "", false, false, false), cob.ExitNotFound)
	})

	t.Run("self-check: verdict prints before the chain (supporting context is a trailer)", func(t *testing.T) {
		// `cob log` is for chain history; `cob diff <coords>` is for the
		// verdict. When the chain rendered above the per-asset comparison
		// the verdict was buried — operators had to scroll past
		// supporting evidence to see whether the check passed. Pin the
		// reordered output so a refactor that puts the chain back on top
		// fails here.
		ca := clitest.NewStatefulCA()
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "thing.bin", "payload")
		mf := clitest.WriteFile(t, dir, "m.yaml",
			"domain: a\nrepository: r\nnamespace: n\npackage: p\nsources:\n  thing: ./thing.bin\n")
		cfg, stdout, _ := clitest.UseFake(t, ca)
		if err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, false, 4); err != nil {
			t.Fatalf("publish setup: %v", err)
		}
		// Reset stdout so we only inspect the self-check output.
		stdout.Reset()
		if err := diff.Run(ctx, cfg, []string{"a/r/n/p@1.0.0"}, "", false, false, false); err != nil {
			t.Fatalf("self-check: %v", err)
		}
		out := stdout.String()
		verdictIdx := strings.Index(out, "OK: every recorded asset still matches")
		chainIdx := strings.Index(out, "chain of evidence:")
		if verdictIdx == -1 {
			t.Fatalf("missing verdict line in:\n%s", out)
		}
		if chainIdx == -1 {
			t.Fatalf("missing chain trailer in:\n%s", out)
		}
		if verdictIdx >= chainIdx {
			t.Errorf("verdict (idx %d) should appear BEFORE chain (idx %d) — chain is supporting context, not the headline:\n%s", verdictIdx, chainIdx, out)
		}
	})

	// --- Dir mode (two args, first is a directory) ----------------------------

	t.Run("dir: terse default — match row carries size, no SHA in scan path", func(t *testing.T) {
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "payload.txt", "hello")
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5, helloSHA)})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg@1.0.0"}, "", false, false, false)
		if err != nil {
			t.Fatalf("diff dir (clean): %v", err)
		}
		out := stdout.String()
		if !strings.Contains(out, "hashing local files; comparing to the published asset SHAs") {
			t.Errorf("header should explain the comparison being done:\n%s", out)
		}
		if !strings.Contains(out, "5 B") {
			t.Errorf("default match line should still show size (5 B):\n%s", out)
		}
		if strings.Contains(out, helloSHA) {
			t.Errorf("default output should NOT include full SHA on match rows (use --verbose):\n%s", out)
		}
	})

	t.Run("dir: --verbose adds full SHA + path as continuation lines", func(t *testing.T) {
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "payload.txt", "hello")
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5, helloSHA)})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg@1.0.0"}, "", false, true, false)
		if err != nil {
			t.Fatalf("diff dir --verbose: %v", err)
		}
		out := stdout.String()
		if !strings.Contains(out, helloSHA) {
			t.Errorf("--verbose should put the full SHA back on match rows:\n%s", out)
		}
		if !strings.Contains(out, "sha256") || !strings.Contains(out, "source") {
			t.Errorf("--verbose should label continuation lines (sha256, source):\n%s", out)
		}
	})

	t.Run("dir: mismatch -> exit 4 with both hashes labeled", func(t *testing.T) {
		dir := t.TempDir()
		clitest.WriteFile(t, dir, "payload.txt", "hello")
		other := "deadbeef0000000000000000000000000000000000000000000000000000beef"
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 5, other)})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg@1.0.0"}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitMismatch)
		out := stdout.String()
		for _, want := range []string{"mismatch", "local", "published", helloSHA, other} {
			if !strings.Contains(out, want) {
				t.Errorf("mismatch row should include %q:\n%s", want, out)
			}
		}
	})

	t.Run("dir: missing local file -> exit 4 with 'missing locally' + published size", func(t *testing.T) {
		dir := t.TempDir() // empty
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{ListAssetsFn: publishedAsset("payload.txt", 4242, helloSHA)})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg@1.0.0"}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitMismatch)
		out := stdout.String()
		if !strings.Contains(out, "missing locally") {
			t.Errorf("expected 'missing locally' for absent file:\n%s", out)
		}
		if !strings.Contains(out, "4.1 KB") { // FormatSize(4242) → "4.1 KB"
			t.Errorf("expected published size shown for missing file:\n%s", out)
		}
	})

	t.Run("dir: file as first arg with second arg → routed-error", func(t *testing.T) {
		// `cob diff some.yaml other.thing` is a typo, not a real
		// invocation. The dispatcher rejects "two args, first is a
		// file" rather than silently misrouting.
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		dir := t.TempDir()
		mf := clitest.WriteFile(t, dir, "m.yaml", "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x\n")
		err := diff.Run(ctx, cfg, []string{mf, "dom/repo/ns/pkg@1.0.0"}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitError)
	})

	t.Run("dir: coordinates without a version → routed-error", func(t *testing.T) {
		dir := t.TempDir()
		cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg"}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitError)
	})

	t.Run("dir: single-dir-arg case gets a shape hint", func(t *testing.T) {
		dir := t.TempDir()
		cfg, _, stderr := clitest.UseFake(t, &clitest.FakeCA{})
		err := diff.Run(ctx, cfg, []string{dir}, "", false, false, false)
		clitest.WantExit(t, err, cob.ExitError)
		if !strings.Contains(stderr.String(), "is a directory") {
			t.Errorf("error should call out the directory-without-coords case:\n%s", stderr.String())
		}
	})

	t.Run("dir: server-supplied traversal-shaped asset name is rejected, never hashed", func(t *testing.T) {
		// CodeArtifact asset names are path-like and server-controlled. A
		// "../../etc/passwd"-style name must be rejected by cliutil.SafeJoin
		// rather than stat'd/hashed under the user-chosen directory. Stage
		// a real file outside the dir at exactly the path the traversal
		// would resolve to, so a regression (raw filepath.Join) would
		// actually succeed at hashing it — the test then fails because
		// the row should be an op-error, not a match.
		outsideDir := t.TempDir()
		clitest.WriteFile(t, outsideDir, "secret.bin", "leaked")
		dir := filepath.Join(outsideDir, "scope")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		traversalName := "../secret.bin"
		cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{
			ListAssetsFn: publishedAsset(traversalName, 6, helloSHA),
		})
		err := diff.Run(ctx, cfg, []string{dir, "dom/repo/ns/pkg@1.0.0"}, "", false, false, false)
		// op-error path: exit 1 (not 0/4) and the row carries the cliutil.SafeJoin reason.
		clitest.WantExit(t, err, cob.ExitError)
		out := stdout.String()
		if !strings.Contains(out, "unsafe asset name") {
			t.Errorf("expected diff to reject the traversal-shaped name:\n%s", out)
		}
		// Summary must reflect "1 could not be checked, 0 matched" — a regression
		// that hashed the outside-the-dir file would land it as a 1-matched row.
		if !strings.Contains(out, "1 could not be checked") || !strings.Contains(out, "(0 matched)") {
			t.Errorf("traversal name must NOT be hashed; summary should report op-error not match:\n%s", out)
		}
	})
}

// oneAsset returns a ListAssetsFn that reports a single named asset with the
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
	clitest.WriteFile(t, dir, "payload.txt", "payload-bytes")
	return clitest.WriteFile(t, dir, "m.yaml",
		"domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  payload: ./payload.txt\n")
}
