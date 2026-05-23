package cliutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/output"
)

// withCwd runs body inside dir, restoring the prior cwd on cleanup.
// Helper for testing the cwd-walking branch of CurrentPackage.
func withCwd(t *testing.T, dir string, body func()) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	body()
}

// withEnv clears + sets an env var for the duration of a test.
func withEnv(t *testing.T, name, value string) {
	t.Helper()
	prev, had := os.LookupEnv(name)
	if value == "" {
		_ = os.Unsetenv(name)
	} else {
		_ = os.Setenv(name, value)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// withHomeRedirect points os.UserConfigDir at a temp dir so the test
// doesn't poke at the developer's real ~/.config. XDG_CONFIG_HOME wins
// on Linux/macOS; HOME is the fallback on systems without XDG.
func withHomeRedirect(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	withEnv(t, "XDG_CONFIG_HOME", tmp)
	withEnv(t, "HOME", tmp)
	return tmp
}

func TestCurrentPackageNothingSet(t *testing.T) {
	withCwd(t, t.TempDir(), func() {
		withHomeRedirect(t)
		coords, src, err := CurrentPackage(&Config{})
		if err != nil {
			t.Fatalf("CurrentPackage error: %v", err)
		}
		if coords != "" || src != SourceUnknown {
			t.Errorf("expected ('', SourceUnknown), got (%q, %q)", coords, src)
		}
	})
}

func TestCurrentPackageFlagWins(t *testing.T) {
	// Even if a cwd .cob/current and home file both exist, an explicit
	// --package flag (mirrored into cfg.PackageOverride before
	// CurrentPackage is called) must win.
	dir := t.TempDir()
	withCwd(t, dir, func() {
		withHomeRedirect(t)
		_, _ = SetCurrentPackage("home/coords/from/disk@1", true)
		_, _ = SetCurrentPackage("cwd/coords/from/disk@1", false)
		cfg := &Config{PackageOverride: "flag/coords@1", PackageOverrideSource: SourceFlag}
		coords, src, _ := CurrentPackage(cfg)
		if coords != "flag/coords@1" {
			t.Errorf("flag should win, got %q", coords)
		}
		if src != SourceFlag {
			t.Errorf("source = %q, want %q", src, SourceFlag)
		}
	})
}

func TestCurrentPackageCwdBeatsHome(t *testing.T) {
	dir := t.TempDir()
	withCwd(t, dir, func() {
		withHomeRedirect(t)
		_, _ = SetCurrentPackage("home/coords@1", true)
		_, _ = SetCurrentPackage("cwd/coords@1", false)
		coords, src, _ := CurrentPackage(&Config{})
		if coords != "cwd/coords@1" {
			t.Errorf("cwd should beat home, got %q", coords)
		}
		// Direct cwd hit prettifies to "./.cob/current" — short and
		// unambiguous because the operator is standing in this dir.
		if string(src) != "./.cob/current" {
			t.Errorf("cwd-direct source = %q, want %q", src, "./.cob/current")
		}
	})
}

func TestCurrentPackageHomeFallback(t *testing.T) {
	dir := t.TempDir() // no .cob here
	withCwd(t, dir, func() {
		home := withHomeRedirect(t)
		_, _ = SetCurrentPackage("home/coords@1", true)
		coords, src, _ := CurrentPackage(&Config{})
		if coords != "home/coords@1" {
			t.Errorf("home fallback should fire, got %q", coords)
		}
		// Home renders under ~ when the path is inside the test's
		// redirected HOME. Tolerate either "~/cob/current" (XDG sets
		// UserConfigDir to $XDG_CONFIG_HOME directly, so homeCobDir is
		// $XDG_CONFIG_HOME/cob) or the absolute path if the renderer
		// couldn't relativize.
		wantSuffix := "cob/current"
		if !strings.HasSuffix(string(src), wantSuffix) {
			t.Errorf("home source = %q, want to end in %q", src, wantSuffix)
		}
		// And it should contain the home base or render under ~.
		if !strings.HasPrefix(string(src), "~/") && !strings.HasPrefix(string(src), home) {
			t.Errorf("home source = %q, expected to start with ~/ or %q", src, home)
		}
	})
}

func TestCurrentPackageWalksUpAndSourceShowsAncestorPath(t *testing.T) {
	// The footgun this test exists to prevent: an ancestor directory has
	// a forgotten .cob/current and you cd into a deep subdirectory of an
	// unrelated project. The no-arg command inherits, the source
	// attribution must NOT just say "from .cob/current" (which directory's?)
	// — it must surface the absolute path of the ancestor that won, so
	// the operator can see the resolution came from somewhere they
	// didn't intend.
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		_, _ = SetCurrentPackage("walked/up@1", false)
	})

	withCwd(t, deep, func() {
		withHomeRedirect(t)
		coords, src, _ := CurrentPackage(&Config{})
		if coords != "walked/up@1" {
			t.Errorf("cwd walk-up didn't find the ancestor's .cob/current, got %q", coords)
		}
		// The cwd is `deep`, the file is at `root/.cob/current`. The
		// source must be the absolute path to that file — not the
		// generic "./.cob/current" (which would falsely suggest the
		// pointer lives in cwd) and not a generic label like
		// "parent .cob/current" (which doesn't say which parent).
		// EvalSymlinks-resolved to handle macOS /var → /private/var.
		wantAbs := filepath.Join(root, ".cob", "current")
		gotResolved, _ := filepath.EvalSymlinks(string(src))
		wantResolved, _ := filepath.EvalSymlinks(wantAbs)
		if gotResolved == "" {
			gotResolved = string(src)
		}
		if wantResolved == "" {
			wantResolved = wantAbs
		}
		if gotResolved != wantResolved {
			t.Errorf("walk-up source = %q, want absolute ancestor path %q — operator must be able to tell *which* ancestor reached up to win", src, wantAbs)
		}
		if string(src) == "./.cob/current" {
			t.Error("walk-up must not be labelled './.cob/current' — that's reserved for the cwd-direct hit; using it for a walk-up hides the ambiguity that motivates this attribution")
		}
	})
}

func TestSetCurrentPackageDropsGitignore(t *testing.T) {
	dir := t.TempDir()
	withCwd(t, dir, func() {
		_, err := SetCurrentPackage("p@1", false)
		if err != nil {
			t.Fatal(err)
		}
		gi, err := os.ReadFile(filepath.Join(dir, ".cob", ".gitignore"))
		if err != nil {
			t.Fatalf("expected .cob/.gitignore to be created on cwd-local SetCurrentPackage: %v", err)
		}
		if !bytes.Contains(gi, []byte("current")) {
			t.Errorf(".gitignore should mention `current`, got %q", gi)
		}
	})
}

func TestSetCurrentPackageDoesNotTrampleExistingGitignore(t *testing.T) {
	dir := t.TempDir()
	withCwd(t, dir, func() {
		if err := os.Mkdir(filepath.Join(dir, ".cob"), 0o700); err != nil {
			t.Fatal(err)
		}
		giPath := filepath.Join(dir, ".cob", ".gitignore")
		custom := []byte("# operator's notes\n*.log\n")
		if err := os.WriteFile(giPath, custom, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SetCurrentPackage("p@1", false); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(giPath)
		if !bytes.Equal(got, custom) {
			t.Errorf("SetCurrentPackage trampled an existing .gitignore; got %q, want %q", got, custom)
		}
	})
}

func TestClearCurrentPackageIdempotent(t *testing.T) {
	dir := t.TempDir()
	withCwd(t, dir, func() {
		// First clear: no file exists yet → must not error.
		if _, err := ClearCurrentPackage(false); err != nil {
			t.Errorf("clear on missing file should be idempotent, got %v", err)
		}
		// Set, then clear, then clear again — none should error.
		_, _ = SetCurrentPackage("p@1", false)
		if _, err := ClearCurrentPackage(false); err != nil {
			t.Fatal(err)
		}
		if _, err := ClearCurrentPackage(false); err != nil {
			t.Errorf("second clear should be idempotent, got %v", err)
		}
	})
}

func TestResolveTargetEmitsNoticeOnFallback(t *testing.T) {
	// When ResolveTarget falls back to a current package, it must emit
	// "Using <coords> (from <source>)" to stderr (Notice), NOT stdout
	// (Plain). Stdout is the script-capture channel for cob resolve
	// (`VERSION=$(cob resolve ...)`); a header line on stdout would
	// silently break that contract.
	dir := t.TempDir()
	withCwd(t, dir, func() {
		_, _ = SetCurrentPackage("p@1", false)
		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		got, err := ResolveTarget(&Config{}, w, nil, "resolve")
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		if got != "p@1" {
			t.Errorf("ResolveTarget = %q, want p@1", got)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout must be empty when ResolveTarget falls back (script-capture channel); got %q", stdout.String())
		}
		if !bytes.Contains(stderr.Bytes(), []byte("Using p@1")) {
			t.Errorf("stderr should carry 'Using p@1' header, got %q", stderr.String())
		}
	})
}

func TestResolveTargetExplicitArgWins(t *testing.T) {
	// When a positional arg is given, ResolveTarget uses it verbatim
	// and emits no header (no implicit fallback happened).
	dir := t.TempDir()
	withCwd(t, dir, func() {
		_, _ = SetCurrentPackage("currentpkg@1", false)
		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		got, err := ResolveTarget(&Config{}, w, []string{"explicit@2"}, "log")
		if err != nil {
			t.Fatal(err)
		}
		if got != "explicit@2" {
			t.Errorf("got %q, want explicit@2 (positional must win)", got)
		}
		if stderr.Len() != 0 {
			t.Errorf("no header when a positional was given; got %q", stderr.String())
		}
	})
}

func TestResolveTargetNoCurrentNoArgErrors(t *testing.T) {
	withCwd(t, t.TempDir(), func() {
		withHomeRedirect(t)
		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		_, err := ResolveTarget(&Config{}, w, nil, "log")
		if err == nil {
			t.Fatal("expected an error when no positional AND no current package")
		}
		// Error message must point at the fix.
		if !bytes.Contains([]byte(err.Error()), []byte("cob use")) {
			t.Errorf("error should mention `cob use`, got %v", err)
		}
	})
}

func TestResolveTargetVersionOnlyOverrideMergesWithCurrent(t *testing.T) {
	// `cob log @latest` against a current package without a version
	// must merge: keep the current's coords, attach @latest.
	dir := t.TempDir()
	withCwd(t, dir, func() {
		withHomeRedirect(t)
		_, _ = SetCurrentPackage("vt-dev/installer-artifacts/vtwriter/vtwriter-installer", false)

		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		got, err := ResolveTarget(&Config{}, w, []string{"@latest"}, "log")
		if err != nil {
			t.Fatalf("ResolveTarget(@latest): %v", err)
		}
		want := "vt-dev/installer-artifacts/vtwriter/vtwriter-installer@latest"
		if got != want {
			t.Errorf("merged coords = %q, want %q", got, want)
		}
		// And the Notice header should make the merge visible.
		if !bytes.Contains(stderr.Bytes(), []byte(want)) {
			t.Errorf("Notice should announce the merged coords %q, got %q", want, stderr.String())
		}
		if !bytes.Contains(stderr.Bytes(), []byte("@latest override")) {
			t.Errorf("Notice should call out '@latest override', got %q", stderr.String())
		}
	})
}

func TestResolveTargetVersionOnlyOverrideReplacesCurrentVersion(t *testing.T) {
	// Current package already has @1, positional is @2 → result is the
	// current's coords with version 2 (the LAST @ wins, mirroring how
	// manifest.ParseCoordinates treats the version separator).
	dir := t.TempDir()
	withCwd(t, dir, func() {
		withHomeRedirect(t)
		_, _ = SetCurrentPackage("vt-dev/installer-artifacts/vtwriter/vtwriter-installer@5.2.1.5", false)

		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		got, _ := ResolveTarget(&Config{}, w, []string{"@5.2.1.4"}, "log")
		want := "vt-dev/installer-artifacts/vtwriter/vtwriter-installer@5.2.1.4"
		if got != want {
			t.Errorf("override should replace the current's version; got %q, want %q", got, want)
		}
	})
}

func TestResolveTargetVersionOnlyOverrideWithoutCurrentErrors(t *testing.T) {
	withCwd(t, t.TempDir(), func() {
		withHomeRedirect(t)
		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		_, err := ResolveTarget(&Config{}, w, []string{"@latest"}, "log")
		if err == nil {
			t.Fatal("@latest with no current package should error helpfully")
		}
		// Error must distinguish "version-only override has nothing to
		// attach to" from "no positional and no current" — same fix
		// (cob use), but different framing.
		if !bytes.Contains([]byte(err.Error()), []byte("version-only override")) {
			t.Errorf("error should mention version-only override; got %v", err)
		}
		if !bytes.Contains([]byte(err.Error()), []byte("cob use")) {
			t.Errorf("error should point at `cob use`; got %v", err)
		}
	})
}

func TestResolveTargetFullPositionalWinsOverCurrent(t *testing.T) {
	// A positional containing '/' is treated as a full coordinate
	// override, regardless of what the current package is. The
	// version-only-override rule only fires when the positional starts
	// with @ AND has no '/' (so a real path like ./manifest.yaml
	// doesn't get rerouted through the merge).
	dir := t.TempDir()
	withCwd(t, dir, func() {
		withHomeRedirect(t)
		_, _ = SetCurrentPackage("a/b/c/d@1", false)

		var stdout, stderr bytes.Buffer
		w := output.NewWithWriters(&stdout, &stderr, output.Mode{})
		got, _ := ResolveTarget(&Config{}, w, []string{"x/y/z/w@2"}, "log")
		if got != "x/y/z/w@2" {
			t.Errorf("full positional should win verbatim, got %q", got)
		}
		// And no Notice — explicit positional, no implicit fallback fired.
		if stderr.Len() != 0 {
			t.Errorf("no Notice expected on explicit full positional, got %q", stderr.String())
		}
	})
}

func TestReadCoordsFileRejectsMultiline(t *testing.T) {
	// A corrupt file with embedded newlines is treated as absent (false),
	// not silently used. Protects against an operator's editor that added
	// a trailing line.
	dir := t.TempDir()
	path := filepath.Join(dir, "current")
	if err := os.WriteFile(path, []byte("good@1\nthen\nsomething\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := readCoordsFile(path); ok {
		t.Errorf("multi-line file should be rejected; got %q, ok=%v", v, ok)
	}
}
