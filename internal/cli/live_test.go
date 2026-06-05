//go:build livetest

// Live integration tests. These drive the *real* cob binary as a subprocess
// against a real AWS CodeArtifact domain — no fakes. They are gated behind the
// `livetest` build tag so `go test ./...` and CI never compile or run them.
//
// Run:
//
//	go test -tags livetest ./internal/cli/ -run Live -v -count=1 -timeout 15m
//
// Requirements:
//   - A resolvable AWS profile with CodeArtifact read/write on the target domain
//     (SSO sessions must be logged in: `aws sso login --profile <p>`).
//   - The Go toolchain on PATH (the suite builds cob from source once).
//
// Configuration (env, with berth-test defaults):
//
//	COB_LIVE_PROFILE  AWS profile         (default: devpitio_dev)
//	COB_LIVE_REGION   AWS region          (default: us-east-2)
//	COB_LIVE_DOMAIN   CodeArtifact domain (default: berth-test)
//
// Blast radius: the lifecycle test only ever touches a uniquely-named
// cob-livetest/e2e-<pid>-<unix> package it creates, and rm's every version it
// makes (across dev/staging/prod) in t.Cleanup. Pre-existing fixtures are read
// only, never mutated.
package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
)

// ---- build-once -------------------------------------------------------------

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

// cobBinary builds cob from source once per test run and returns the path to
// the compiled binary. Building from source (rather than assuming a $PATH
// install) guarantees the tests exercise the working tree, not a stale binary.
func cobBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cob-livebin-")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "cob")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/jmurray2011/cob/cmd/cob")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building cob: %w\n%s", err, out)
			return
		}
		buildBin = bin
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return buildBin
}

// ---- live config + runner ---------------------------------------------------

type liveCfg struct {
	profile, region, domain string
}

func live() liveCfg {
	return liveCfg{
		profile: envOr("COB_LIVE_PROFILE", "devpitio_dev"),
		region:  envOr("COB_LIVE_REGION", "us-east-2"),
		domain:  envOr("COB_LIVE_DOMAIN", "berth-test"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// result is the captured outcome of one cob subprocess invocation.
type result struct {
	stdout, stderr string
	exit           int
}

// execCob runs the cob binary with exactly args (no globals injected) and
// captures stdout/stderr/exit. A non-ExitError failure (binary missing, etc.)
// is fatal — that's a harness problem, not a cob behavior under test.
func execCob(t *testing.T, args ...string) result {
	t.Helper()
	bin := cobBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ() // inherit HOME so ~/.aws + SSO token cache resolve
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("exec cob %v: %v", args, err)
		}
	}
	return result{stdout: so.String(), stderr: se.String(), exit: exit}
}

// run invokes cob with the live profile/region and --no-tui prepended to args.
func (lc liveCfg) run(t *testing.T, args ...string) result {
	t.Helper()
	full := append([]string{"--profile", lc.profile, "--region", lc.region, "--no-tui"}, args...)
	return execCob(t, full...)
}

// deletePackageEverywhere removes the scratch package — every version and the
// package container itself — from all three repos via the CodeArtifact SDK.
// cob rm can't delete a container (it operates on versions), so teardown drops
// to the SDK. Best-effort: it never fails the test, since t.Cleanup runs after
// the assertions have already reported.
func (lc liveCfg) deletePackageEverywhere(t *testing.T, namespace, pkg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithSharedConfigProfile(lc.profile),
		awsconfig.WithRegion(lc.region))
	if err != nil {
		t.Logf("cleanup: load aws config: %v", err)
		return
	}
	ca := codeartifact.NewFromConfig(awsCfg)
	for _, repo := range []string{"prod", "staging", "dev"} {
		_, err := ca.DeletePackage(ctx, &codeartifact.DeletePackageInput{
			Domain:     aws.String(lc.domain),
			Repository: aws.String(repo),
			Format:     catypes.PackageFormatGeneric,
			Namespace:  aws.String(namespace),
			Package:    aws.String(pkg),
		})
		if err != nil {
			var notFound *catypes.ResourceNotFoundException
			if !errors.As(err, &notFound) {
				t.Logf("cleanup: delete-package %s/%s/%s: %v", repo, namespace, pkg, err)
			}
		}
	}
}

func (r result) mustExit(t *testing.T, want int, what string) {
	t.Helper()
	if r.exit != want {
		t.Fatalf("%s: exit=%d want=%d\n--- stdout ---\n%s\n--- stderr ---\n%s",
			what, r.exit, want, r.stdout, r.stderr)
	}
}

func mustJSON(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("parsing JSON: %v\n%s", err, raw)
	}
}

// ---- read-only smoke --------------------------------------------------------

// TestLiveReadOnlySmoke confirms credentials resolve and the target domain is
// reachable, exercising the listing paths with zero mutations. If this fails,
// every other live test will too — it's the canary.
func TestLiveReadOnlySmoke(t *testing.T) {
	lc := live()

	t.Run("ls domains shows the target domain", func(t *testing.T) {
		r := lc.run(t, "ls")
		r.mustExit(t, cob.ExitOK, "ls domains")
		if !strings.Contains(r.stdout, lc.domain) {
			t.Errorf("ls domains output missing %q:\n%s", lc.domain, r.stdout)
		}
	})

	t.Run("ls repos returns at least one repository", func(t *testing.T) {
		r := lc.run(t, "ls", lc.domain, "--json")
		r.mustExit(t, cob.ExitOK, "ls repos --json")
		var repos []string
		mustJSON(t, r.stdout, &repos)
		if len(repos) == 0 {
			t.Fatalf("expected at least one repo in %s", lc.domain)
		}
	})

	t.Run("tree of the domain renders without error", func(t *testing.T) {
		lc.run(t, "tree", lc.domain).mustExit(t, cob.ExitOK, "tree domain")
	})

	t.Run("--verbose emits a step trace on stderr", func(t *testing.T) {
		r := lc.run(t, "ls", lc.domain, "--verbose")
		r.mustExit(t, cob.ExitOK, "ls --verbose")
		// The verbose channel is leveled ("verbose:") and lives on stderr so
		// it never pollutes stdout/--json. One line per AWS call is emitted.
		if !strings.Contains(r.stderr, "verbose:") {
			t.Errorf("--verbose produced no 'verbose:' line on stderr:\n%s", r.stderr)
		}
		if strings.Contains(r.stdout, "verbose:") {
			t.Errorf("--verbose leaked onto stdout:\n%s", r.stdout)
		}
	})
}

// ---- exit-code matrix -------------------------------------------------------

// TestLiveExitCodes pins the exit codes scripts depend on. Subprocess execution
// is the only way to prove the real os.Exit value (an in-process Run() returns
// an *ExitError that main translates — here we test the translation too).
func TestLiveExitCodes(t *testing.T) {
	lc := live()
	missing := lc.domain + "/dev/cob-livetest/does-not-exist-" + unique()

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"version flag", []string{"--version"}, cob.ExitOK},
		{"resolve missing package", []string{"resolve", missing}, cob.ExitNotFound},
		{"ls missing package", []string{"ls", missing}, cob.ExitNotFound},
		{"rm refuses @latest", []string{"rm", missing + "@latest", "--force", "--yes"}, cob.ExitError},
		{"invalid coordinates", []string{"ls", "this is not a coord"}, cob.ExitError},
		{"publish missing manifest", []string{"publish", "/no/such/manifest.yaml", "--version", "1.0.0", "--yes"}, cob.ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc.run(t, tc.args...).mustExit(t, tc.want, tc.name)
		})
	}
}

// ---- full lifecycle ---------------------------------------------------------

// TestLiveLifecycle drives the entire cob lifecycle against live CodeArtifact:
// init -> lint -> dry-run -> publish -> read-backs (resolve/ls/log/manifest/
// diff) -> pull + round-trip diff -> drift detection -> promote dev->staging->
// prod -> promotion status -> cross-repo byte-identity -> rm safety gate ->
// cleanup. Every created version is rm'd in t.Cleanup regardless of where the
// test stops, so a mid-run failure never leaves litter in the domain.
func TestLiveLifecycle(t *testing.T) {
	lc := live()
	const version = "1.0.0"
	pkg := "e2e-" + unique()
	nsPkg := "cob-livetest/" + pkg

	coord := func(repo string) string {
		return fmt.Sprintf("%s/%s/%s@%s", lc.domain, repo, nsPkg, version)
	}
	noVer := func(repo string) string {
		return fmt.Sprintf("%s/%s/%s", lc.domain, repo, nsPkg)
	}

	// Cleanup is registered before any mutation so a failure at any stage
	// still leaves no trace. cob rm is version-level only (DeletePackageVersions)
	// — deleting the last version leaves an empty package *container* that cob
	// has no verb to remove. Teardown therefore uses the SDK's DeletePackage,
	// which drops every version AND the container, in whatever state the test
	// stopped in. (The lifecycle's own rm subtests still exercise cob rm; this
	// is belt-and-suspenders.)
	t.Cleanup(func() { lc.deletePackageEverywhere(t, "cob-livetest", pkg) })

	// Stage a directory of source files; cob init scaffolds the manifest.
	dir := t.TempDir()
	writeFile(t, dir, "alpha.txt", "alpha payload\n")
	writeFile(t, dir, "beta.bin", "beta payload bytes\n")
	manifestPath := filepath.Join(dir, "cob-manifest.yaml")

	t.Run("init scaffolds a manifest from the directory", func(t *testing.T) {
		r := lc.run(t, "init", dir, "--for", noVer("dev"))
		r.mustExit(t, cob.ExitOK, "init")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("manifest not written: %v", err)
		}
		if !strings.Contains(string(data), "domain: "+lc.domain) {
			t.Errorf("manifest missing target domain:\n%s", data)
		}
	})

	t.Run("diff lints the generated manifest offline", func(t *testing.T) {
		lc.run(t, "diff", manifestPath).mustExit(t, cob.ExitOK, "diff lint")
	})

	t.Run("publish --dry-run verifies sources without publishing", func(t *testing.T) {
		lc.run(t, "publish", manifestPath, "--version", version, "--dry-run").
			mustExit(t, cob.ExitOK, "publish --dry-run")
		// A dry run must not create the version.
		lc.run(t, "resolve", noVer("dev")).mustExit(t, cob.ExitNotFound, "resolve after dry-run")
	})

	t.Run("publish lands two assets and a finalized provenance", func(t *testing.T) {
		r := lc.run(t, "publish", manifestPath, "--version", version, "--yes", "--json")
		r.mustExit(t, cob.ExitOK, "publish")
		var res cob.CommandResult
		mustJSON(t, r.stdout, &res)
		if res.Status != "ok" {
			t.Errorf("publish status=%q want ok", res.Status)
		}
		if len(res.Assets) < 2 {
			t.Errorf("publish recorded %d assets, want >=2", len(res.Assets))
		}
	})

	t.Run("re-publishing the same version conflicts without --force", func(t *testing.T) {
		lc.run(t, "publish", manifestPath, "--version", version, "--yes").
			mustExit(t, cob.ExitConflict, "publish duplicate")
	})

	t.Run("resolve reports the published version", func(t *testing.T) {
		r := lc.run(t, "resolve", noVer("dev"))
		r.mustExit(t, cob.ExitOK, "resolve")
		if got := strings.TrimSpace(r.stdout); got != version {
			t.Errorf("resolve = %q, want %q", got, version)
		}
	})

	t.Run("ls assets lists both sources plus the provenance doc", func(t *testing.T) {
		r := lc.run(t, "ls", coord("dev"), "--json")
		r.mustExit(t, cob.ExitOK, "ls assets --json")
		var assets []cob.AssetSummary
		mustJSON(t, r.stdout, &assets)
		names := map[string]bool{}
		for _, a := range assets {
			names[a.Name] = true
		}
		for _, want := range []string{"alpha.txt", "beta.bin", cob.ProvenanceFile} {
			if !names[want] {
				t.Errorf("ls assets missing %q; got %v", want, keys(names))
			}
		}
	})

	t.Run("log shows a single publish chain event", func(t *testing.T) {
		r := lc.run(t, "log", coord("dev"), "--json")
		r.mustExit(t, cob.ExitOK, "log --json")
		var prov cob.Provenance
		mustJSON(t, r.stdout, &prov)
		if len(prov.Chain) != 1 || prov.Chain[0].Event != "publish" {
			t.Errorf("expected one publish chain event, got %+v", prov.Chain)
		}
	})

	t.Run("manifest reconstructs from the published version", func(t *testing.T) {
		r := lc.run(t, "manifest", coord("dev"))
		r.mustExit(t, cob.ExitOK, "manifest")
		if !strings.Contains(r.stdout, "alpha.txt") || !strings.Contains(r.stdout, "beta.bin") {
			t.Errorf("reconstructed manifest missing sources:\n%s", r.stdout)
		}
	})

	t.Run("diff self-integrity passes on a fresh publish", func(t *testing.T) {
		lc.run(t, "diff", coord("dev")).mustExit(t, cob.ExitOK, "diff self-check")
	})

	pulled := t.TempDir()
	t.Run("pull restores the exact published bytes", func(t *testing.T) {
		lc.run(t, "pull", coord("dev"), pulled+"/").mustExit(t, cob.ExitOK, "pull")
		for name, want := range map[string]string{
			"alpha.txt": "alpha payload\n",
			"beta.bin":  "beta payload bytes\n",
		} {
			got, err := os.ReadFile(filepath.Join(pulled, name))
			if err != nil {
				t.Errorf("pulled %s missing: %v", name, err)
				continue
			}
			if string(got) != want {
				t.Errorf("pulled %s = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("diff dir vs published is clean right after pull", func(t *testing.T) {
		lc.run(t, "diff", pulled, coord("dev")).mustExit(t, cob.ExitOK, "diff dir")
	})

	t.Run("tampering a pulled file surfaces as drift", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(pulled, "alpha.txt"), []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
		lc.run(t, "diff", pulled, coord("dev")).mustExit(t, cob.ExitMismatch, "diff drift")
	})

	t.Run("promote dev->staging->prod preserves bytes and extends the chain", func(t *testing.T) {
		lc.run(t, "promote", coord("dev"), "--to", "staging", "--yes").
			mustExit(t, cob.ExitOK, "promote to staging")
		lc.run(t, "promote", coord("staging"), "--to", "prod", "--yes").
			mustExit(t, cob.ExitOK, "promote to prod")

		// prod's provenance must carry the full lineage: publish + 2 promotes.
		r := lc.run(t, "log", coord("prod"), "--json")
		r.mustExit(t, cob.ExitOK, "log prod --json")
		var prov cob.Provenance
		mustJSON(t, r.stdout, &prov)
		if len(prov.Chain) != 3 {
			t.Fatalf("prod chain = %d events, want 3 (publish + 2 promotes): %+v", len(prov.Chain), prov.Chain)
		}
		if prov.Chain[0].Event != "publish" || prov.Chain[1].Event != "promote" || prov.Chain[2].Event != "promote" {
			t.Errorf("prod chain events out of order: %+v", prov.Chain)
		}
	})

	t.Run("ls promotion status shows the version in all three repos", func(t *testing.T) {
		wild := fmt.Sprintf("%s/*/%s@%s", lc.domain, nsPkg, version)
		r := lc.run(t, "ls", wild, "--json")
		r.mustExit(t, cob.ExitOK, "ls promotion --json")
		var statuses []cob.PromotionStatus
		mustJSON(t, r.stdout, &statuses)
		seen := map[string]string{}
		for _, s := range statuses {
			seen[s.Repository] = s.Status
		}
		for _, repo := range []string{"dev", "staging", "prod"} {
			if seen[repo] != "Published" {
				t.Errorf("repo %s status=%q, want Published (all: %+v)", repo, seen[repo], statuses)
			}
		}
	})

	t.Run("diff dev vs prod is byte-identical after promote", func(t *testing.T) {
		lc.run(t, "diff", coord("dev"), coord("prod")).
			mustExit(t, cob.ExitOK, "diff dev vs prod")
	})

	t.Run("rm --force refuses while downstream copies exist", func(t *testing.T) {
		// dev was promoted to staging and prod; deleting it with --force (but
		// not --everywhere) must refuse with ExitConflict so the downstream
		// chains aren't silently orphaned.
		r := lc.run(t, "rm", coord("dev"), "--force", "--yes")
		r.mustExit(t, cob.ExitConflict, "rm refuses downstream")
		// And the version must still be there.
		lc.run(t, "resolve", noVer("dev")).mustExit(t, cob.ExitOK, "dev survives refused rm")
	})

	t.Run("rm --force --everywhere deletes each copy", func(t *testing.T) {
		for _, repo := range []string{"prod", "staging", "dev"} {
			lc.run(t, "rm", coord(repo), "--force", "--everywhere", "--yes").
				mustExit(t, cob.ExitOK, "rm "+repo)
		}
		// Gone everywhere now.
		lc.run(t, "resolve", noVer("dev")).mustExit(t, cob.ExitNotFound, "dev gone")
		lc.run(t, "resolve", noVer("prod")).mustExit(t, cob.ExitNotFound, "prod gone")
	})
}

// ---- small helpers ----------------------------------------------------------

// unique returns a per-process, per-second token for scratch package names so
// concurrent runs (or a re-run after a crash) never collide.
func unique() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().Unix())
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
