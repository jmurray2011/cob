package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

// initDir builds a temp directory pre-populated with a set of files
// and returns the directory path. Used across init tests so each
// scenario can describe the directory it expects to scan.
func initDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

// readManifest loads the manifest cob init wrote into <dir>/cob-manifest.yaml.
// The default output location is part of init's contract; if that moves,
// every test here gets the regression for free.
func readManifest(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "cob-manifest.yaml"))
	if err != nil {
		t.Fatalf("read generated manifest: %v", err)
	}
	return b
}

func TestRunInitFromDirGeneratesSourcesForEachFile(t *testing.T) {
	// Three real files in the dir. init must write a sources block
	// keyed by basename, with ./<name> values, sorted alphabetically.
	dir := initDir(t, map[string]string{
		"app.tar.gz":  "binary",
		"config.yaml": "key: value",
		"README.md":   "hi",
	})
	cfg, _, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, dir, "", "", false, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := string(readManifest(t, dir))
	for _, want := range []string{
		"sources:",
		"app.tar.gz:",
		"./app.tar.gz",
		"config.yaml:",
		"./config.yaml",
		"README.md:",
		"./README.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q in:\n%s", want, got)
		}
	}
	// Sort order — alphabetical, so re-runs produce stable manifests.
	app := strings.Index(got, "app.tar.gz:")
	cfgIdx := strings.Index(got, "config.yaml:")
	readme := strings.Index(got, "README.md:")
	if !(readme < app && app < cfgIdx) {
		// Sorted alphabetically: README.md, app.tar.gz, config.yaml
		// (uppercase < lowercase in Go's default string sort).
		t.Errorf("sources should be alphabetically sorted; got order README=%d app=%d config=%d", readme, app, cfgIdx)
	}
}

func TestRunInitSkipsCobAndHiddenAndDirs(t *testing.T) {
	// The skip rules are part of init's contract: re-running on a
	// pulled directory (which already has cob-manifest.yaml and
	// cob-provenance.json) must not recurse on its own metadata.
	dir := initDir(t, map[string]string{
		"app.bin":             "real asset",
		"cob-manifest.yaml":   "skip: me",     // pulled-manifest convention
		"cob-provenance.json": "{}",           // provenance asset
		".gitignore":          "node_modules", // hidden file
		".env":                "SECRET=oops",  // hidden, would never be a package source anyway
	})
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir/nested.bin"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, _ := useFake(t, &fakeCA{})
	// Use -o to a separate file so init doesn't refuse-overwrite on
	// the cob-manifest.yaml the test fixture seeded.
	outPath := filepath.Join(t.TempDir(), "out.yaml")
	if err := runInit(cfg, dir, "", outPath, false, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, "app.bin") {
		t.Errorf("real asset missing from manifest:\n%s", gotStr)
	}
	for _, mustNotAppear := range []string{
		"cob-manifest.yaml",
		"cob-provenance.json",
		".gitignore",
		".env",
		"subdir",
		"nested.bin",
	} {
		if strings.Contains(gotStr, mustNotAppear) {
			t.Errorf("manifest must skip %q but it appeared:\n%s", mustNotAppear, gotStr)
		}
	}
}

func TestRunInitForPreFillsCoords(t *testing.T) {
	dir := initDir(t, map[string]string{"app.bin": "x"})
	cfg, _, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, dir, "acme/dev/tools/my-app", "", false, false); err != nil {
		t.Fatalf("init --for: %v", err)
	}
	got := string(readManifest(t, dir))
	for _, want := range []string{
		"domain: acme",
		"repository: dev",
		"namespace: tools",
		"package: my-app",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--for should fill %q:\n%s", want, got)
		}
	}
}

func TestRunInitForRejectsVersion(t *testing.T) {
	// --for is for coords only — the version lives on `cob publish`,
	// not in the manifest. Putting @X on --for is a usage error.
	dir := initDir(t, map[string]string{"app.bin": "x"})
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runInit(cfg, dir, "acme/dev/tools/my-app@1.0.0", "", false, false)
	wantExit(t, err, cob.ExitError)
}

func TestRunInitRefusesEmptyDir(t *testing.T) {
	dir := t.TempDir() // no eligible files
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runInit(cfg, dir, "", "", false, false)
	wantExit(t, err, cob.ExitError)
}

func TestRunInitRefusesNonDir(t *testing.T) {
	// Pointing at a file (not a directory) is a clear user error;
	// catch it explicitly rather than tripping over os.ReadDir's
	// "not a directory" deeper in the call.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "thing.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runInit(cfg, filePath, "", "", false, false)
	wantExit(t, err, cob.ExitError)
}

func TestRunInitRefusesExistingManifestWithoutForce(t *testing.T) {
	// Safety against accidental overwrite. Common case: user ran
	// `cob pull` (which writes cob-manifest.yaml), then absent-mindedly
	// runs `cob init .`. We refuse with ExitConflict, signaling "the
	// thing you'd clobber matters; opt in with --force."
	dir := initDir(t, map[string]string{
		"app.bin":           "real",
		"cob-manifest.yaml": "domain: x\nrepository: y\nnamespace: z\npackage: w\n",
	})
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runInit(cfg, dir, "", "", false, false)
	wantExit(t, err, cob.ExitConflict)
	// And the existing manifest should be untouched (still the seed).
	got, err := os.ReadFile(filepath.Join(dir, "cob-manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "domain: x") {
		t.Errorf("existing manifest was modified without --force; got %s", got)
	}
}

func TestRunInitForceOverwrites(t *testing.T) {
	dir := initDir(t, map[string]string{
		"app.bin":           "real",
		"cob-manifest.yaml": "stale: yes\n",
	})
	cfg, _, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, dir, "", "", true, false); err != nil {
		t.Fatalf("init --force: %v", err)
	}
	got := string(readManifest(t, dir))
	if strings.Contains(got, "stale: yes") {
		t.Errorf("--force should have overwritten the stale manifest:\n%s", got)
	}
	if !strings.Contains(got, "app.bin") {
		t.Errorf("--force overwrite should include real source:\n%s", got)
	}
}

func TestRunInitStdoutLeavesNoFile(t *testing.T) {
	// -o - writes the manifest to stdout (consumed by useFake's
	// buffer). The directory should still NOT have a cob-manifest.yaml
	// — that's the whole point of -o -.
	dir := initDir(t, map[string]string{"app.bin": "x"})
	cfg, stdout, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, dir, "", "-", false, false); err != nil {
		t.Fatalf("init -o -: %v", err)
	}
	if !strings.Contains(stdout.String(), "app.bin") {
		t.Errorf("stdout output missing app.bin source:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "cob-manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("-o - should NOT write a file into the dir (stat err=%v)", err)
	}
}

func TestRunInitMinimalOmitsCommentsAndPromote(t *testing.T) {
	dir := initDir(t, map[string]string{"app.bin": "x"})
	cfg, _, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, dir, "acme/dev/tools/my-app", "", false, true); err != nil {
		t.Fatalf("init --minimal: %v", err)
	}
	got := string(readManifest(t, dir))
	if strings.Contains(got, "#") {
		t.Errorf("--minimal should drop comments:\n%s", got)
	}
	if strings.Contains(got, "promote:") {
		t.Errorf("--minimal should drop promote stages:\n%s", got)
	}
	if !strings.Contains(got, "domain: acme") {
		t.Errorf("--minimal still needs schema fields:\n%s", got)
	}
}

// TestInitOutputLoadsAndLints is the contract: whatever `cob init`
// writes must (1) parse via manifest.Load and (2) pass the same
// validateManifest pre-flight that publish/promote/pull/diff use. A
// regression in the template that produced invalid YAML, missing
// local files, or basename collisions would land here before biting
// a first-run user.
func TestInitOutputLoadsAndLints(t *testing.T) {
	cases := []struct {
		name    string
		coords  string
		minimal bool
	}{
		{"placeholders", "", false},
		{"with --for", "acme/dev/tools/my-app", false},
		{"minimal", "acme/dev/tools/my-app", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := initDir(t, map[string]string{
				"a.bin":    "1",
				"b.txt":    "2",
				"sub.yaml": "key: val", // a legitimate yaml asset (not cob-manifest)
			})
			cfg, _, _ := useFake(t, &fakeCA{})
			if err := runInit(cfg, dir, c.coords, "", false, c.minimal); err != nil {
				t.Fatalf("init: %v", err)
			}
			path := filepath.Join(dir, "cob-manifest.yaml")
			m, err := manifest.Load(path)
			if err != nil {
				t.Fatalf("manifest.Load: %v", err)
			}
			if err := validateManifest(m, ""); err != nil {
				t.Fatalf("validateManifest on generated manifest: %v", err)
			}
		})
	}
}
