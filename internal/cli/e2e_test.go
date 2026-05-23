package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cli/diff"
	"github.com/jmurray2011/cob/internal/cli/promote"
	"github.com/jmurray2011/cob/internal/cli/publish"
	"github.com/jmurray2011/cob/internal/cli/pull"
	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// TestEndToEndPublishPullDiffPromote drives the full cob lifecycle against
// an in-memory CodeArtifact fake — publish a manifest, pull the result,
// diff the manifest against the published version, then promote it to a
// second repo and diff the two versions. Each step asserts what the next
// one needs (bytes landed, hashes match, chain extends correctly), so a
// regression anywhere in the lifecycle (an AWS-SDK refactor, a manifest
// schema break, a provenance shape change, the per-command sub-package
// split) lights up one row in this test instead of being discovered
// mid-CI on a real account.
//
// Coverage exists to catch the wiring; per-command edge cases are still
// covered by the focused tests in each sub-package.
func TestEndToEndPublishPullDiffPromote(t *testing.T) {
	ctx := context.Background()
	ca := clitest.NewStatefulCA()
	cfg, _, _ := clitest.UseFake(t, ca)

	// Stage: write a manifest with two local sources next to it.
	dir := t.TempDir()
	clitest.WriteFile(t, dir, "app.bin", "hello world")
	clitest.WriteFile(t, dir, "config.yaml", "key: value\n")
	mf := clitest.WriteFile(t, dir, "manifest.yaml",
		"domain: acme\n"+
			"repository: dev\n"+
			"namespace: tools\n"+
			"package: app\n"+
			"sources:\n"+
			"  app: ./app.bin\n"+
			"  cfg: ./config.yaml\n"+
			"promote:\n"+
			"  stages: [dev, staging, prod]\n")

	// --- 1. Publish ---------------------------------------------------------

	t.Run("publish lands real bytes and a finalizer", func(t *testing.T) {
		if err := publish.Run(ctx, cfg, mf, "1.0.0", false, false, true, false, 4); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// The two declared assets must be in CodeArtifact at the exact
		// bytes the manifest sources held.
		if got, want := string(ca.AssetBytes("acme", "dev", "tools", "app", "1.0.0", "app.bin")), "hello world"; got != want {
			t.Errorf("app.bin post-publish: got %q, want %q", got, want)
		}
		if got, want := string(ca.AssetBytes("acme", "dev", "tools", "app", "1.0.0", "config.yaml")), "key: value\n"; got != want {
			t.Errorf("config.yaml post-publish: got %q, want %q", got, want)
		}
		// The provenance finalizer must be present and the version
		// flipped to Published (StatefulCA does that on a non-Unfinished
		// PublishPackageVersion call).
		provBytes := ca.AssetBytes("acme", "dev", "tools", "app", "1.0.0", cob.ProvenanceFile)
		if provBytes == nil {
			t.Fatal("cob-provenance.json was never written; finalize step did not run")
		}
		var prov cob.Provenance
		if err := json.Unmarshal(provBytes, &prov); err != nil {
			t.Fatalf("provenance is not valid JSON: %v", err)
		}
		if len(prov.Chain) != 1 || prov.Chain[0].Event != "publish" {
			t.Errorf("expected one 'publish' chain event, got %+v", prov.Chain)
		}
		if len(prov.Assets) != 2 {
			t.Errorf("expected 2 recorded assets in provenance, got %d (%+v)", len(prov.Assets), prov.Assets)
		}
	})

	// --- 2. Diff (manifest vs published) -----------------------------------

	t.Run("diff manifest vs published reports clean", func(t *testing.T) {
		if err := diff.Run(ctx, cfg, []string{mf}, "1.0.0", true, false, false); err != nil {
			t.Errorf("diff right after publish should be clean, got %v", err)
		}
	})

	// --- 3. Pull -----------------------------------------------------------

	t.Run("pull restores the exact bytes that were published", func(t *testing.T) {
		dest := t.TempDir()
		if err := pull.Run(ctx, cfg, "acme/dev/tools/app@1.0.0", "", dest, "", "", 4); err != nil {
			t.Fatalf("pull: %v", err)
		}
		for name, want := range map[string]string{
			"app.bin":     "hello world",
			"config.yaml": "key: value\n",
		} {
			got, err := os.ReadFile(filepath.Join(dest, name))
			if err != nil {
				t.Errorf("pulled file %s missing: %v", name, err)
				continue
			}
			if string(got) != want {
				t.Errorf("pulled %s: got %q, want %q", name, got, want)
			}
		}
		// Pull on a directory target also writes cob-manifest.yaml so a
		// downstream re-publish can feed cob without hand-reconstructing.
		if _, err := os.Stat(filepath.Join(dest, "cob-manifest.yaml")); err != nil {
			t.Errorf("expected cob-manifest.yaml to be written next to pulled assets: %v", err)
		}
	})

	// --- 4. Diff (local dir vs published) ----------------------------------

	t.Run("diff dir vs published is clean immediately after pull", func(t *testing.T) {
		dest := t.TempDir()
		if err := pull.Run(ctx, cfg, "acme/dev/tools/app@1.0.0", "", dest, "", "", 4); err != nil {
			t.Fatalf("setup pull: %v", err)
		}
		if err := diff.Run(ctx, cfg, []string{dest, "acme/dev/tools/app@1.0.0"}, "", false, false, false); err != nil {
			t.Errorf("dir-mode diff right after pull should be clean, got %v", err)
		}
	})

	// --- 5. Promote dev -> staging -----------------------------------------

	t.Run("promote extends the chain and preserves bytes", func(t *testing.T) {
		if err := promote.Run(ctx, cfg, "acme/dev/tools/app@1.0.0", "", "staging", false, true, false, false, 4); err != nil {
			t.Fatalf("promote: %v", err)
		}
		// Promoted bytes must be identical at the dest.
		if got, want := string(ca.AssetBytes("acme", "staging", "tools", "app", "1.0.0", "app.bin")), "hello world"; got != want {
			t.Errorf("app.bin post-promote: got %q, want %q (promote must preserve bytes)", got, want)
		}
		// The chain in the dest version's provenance must show both the
		// original publish and the new promote event — a fresh "publish"
		// chain at the dest would mean we lost lineage.
		destProv := ca.AssetBytes("acme", "staging", "tools", "app", "1.0.0", cob.ProvenanceFile)
		if destProv == nil {
			t.Fatal("dest provenance missing after promote")
		}
		var p cob.Provenance
		if err := json.Unmarshal(destProv, &p); err != nil {
			t.Fatalf("dest provenance: %v", err)
		}
		if len(p.Chain) != 2 {
			t.Fatalf("expected chain to have grown to 2 events (publish + promote), got %d: %+v", len(p.Chain), p.Chain)
		}
		if p.Chain[0].Event != "publish" || p.Chain[1].Event != "promote" {
			t.Errorf("chain events out of order: %+v", p.Chain)
		}
		if p.Chain[1].From != "acme/dev" || p.Chain[1].To != "acme/staging" {
			t.Errorf("promote event coords wrong: from=%q to=%q", p.Chain[1].From, p.Chain[1].To)
		}
	})

	// --- 6. Diff version vs version (cross-repo, post-promote) -------------

	t.Run("diff dev@1.0.0 vs staging@1.0.0 is byte-identical", func(t *testing.T) {
		// Excludes cob-provenance.json (the chain trivially differs).
		// Real package bytes must match — that's the whole point of
		// promote: copy, don't re-derive.
		if err := diff.Run(ctx, cfg,
			[]string{"acme/dev/tools/app@1.0.0", "acme/staging/tools/app@1.0.0"},
			"", false, false, false); err != nil {
			t.Errorf("dev vs staging should be byte-identical post-promote, got %v", err)
		}
	})

	// --- 7. Self-check on the promoted version -----------------------------

	t.Run("self-check on staging matches the recorded provenance", func(t *testing.T) {
		if err := diff.Run(ctx, cfg,
			[]string{"acme/staging/tools/app@1.0.0"},
			"", false, false, false); err != nil {
			t.Errorf("self-check on promoted version should pass: %v", err)
		}
	})

	// --- 8. Drift detection: mutate the source after publish, expect diff to scream

	t.Run("editing a local source surfaces as drift", func(t *testing.T) {
		// Overwrite app.bin so the manifest source no longer hashes to
		// the published asset. The next diff must exit ExitMismatch.
		if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := diff.Run(ctx, cfg, []string{mf}, "1.0.0", true, false, false)
		clitest.WantExit(t, err, cob.ExitMismatch)
		// And restore so later subtests (if any are added) still see the
		// pristine input.
		if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("hello world"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	// --- 9. Pull rejects a server-supplied traversal name ------------------

	t.Run("safeJoin defense holds across pull", func(t *testing.T) {
		// Seed a hostile version: an asset whose stored name is "../escape".
		// pull must reject the row (via safeJoin) rather than writing
		// outside the chosen output directory.
		ca.Seed("acme", "dev", "tools", "evil", "0.0.1",
			map[string][]byte{"../escape.bin": []byte("should not land")})
		dest := t.TempDir()
		err := pull.Run(ctx, cfg, "acme/dev/tools/evil@0.0.1", "", dest, "", "", 4)
		if err == nil {
			t.Fatal("pull should fail on a traversal-shaped asset name")
		}
		// Nothing was written outside dest.
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escape.bin")); statErr == nil {
			t.Error("pull wrote outside the output dir despite safeJoin")
		}
		// Useful failure-mode signal: error mentions "escape" or "asset" name.
		if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "exit status") {
			// Either is fine — Pull wraps its asset-level errors into a
			// command-level ExitError but the asset name should surface
			// through the writer; here we just want SOMETHING blame-worthy.
			t.Logf("pull error: %v", err)
		}
	})
}
