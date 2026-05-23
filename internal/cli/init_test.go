package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func TestRunInitDefault(t *testing.T) {
	cfg, stdout, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, "", false); err != nil {
		t.Fatalf("init: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"domain: my-domain",
		"repository: dev",
		"namespace: my-namespace",
		"package: my-package",
		"sources:",
		"promote:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init default output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunInitWithFullCoords(t *testing.T) {
	cfg, stdout, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, "acme/dev/tools/my-app", false); err != nil {
		t.Fatalf("init: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"domain: acme",
		"repository: dev",
		"namespace: tools",
		"package: my-app",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init w/ coords output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunInitWithPartialCoords(t *testing.T) {
	// Domain only — repository/namespace/package keep their placeholders.
	cfg, stdout, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, "acme", false); err != nil {
		t.Fatalf("init: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "domain: acme") {
		t.Errorf("expected domain override, got:\n%s", out)
	}
	if !strings.Contains(out, "namespace: my-namespace") {
		t.Errorf("expected namespace placeholder retained, got:\n%s", out)
	}
}

func TestRunInitMinimalOmitsCommentsAndPromote(t *testing.T) {
	cfg, stdout, _ := useFake(t, &fakeCA{})
	if err := runInit(cfg, "acme/dev/tools/my-app", true); err != nil {
		t.Fatalf("init: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "#") {
		t.Errorf("--minimal should drop comments:\n%s", out)
	}
	if strings.Contains(out, "promote:") {
		t.Errorf("--minimal should drop promote stages:\n%s", out)
	}
	if !strings.Contains(out, "domain: acme") {
		t.Errorf("--minimal still needs schema fields:\n%s", out)
	}
}

func TestRunInitRejectsVersion(t *testing.T) {
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runInit(cfg, "acme/dev/tools/my-app@1.0.0", false)
	wantExit(t, err, cob.ExitError)
}

// TestInitOutputParsesAsManifest is the contract: whatever `cob init`
// writes must be loadable by manifest.Load without error. Regressions in
// the template that produce invalid YAML/schema would surface here
// instead of biting a first-run user.
func TestInitOutputParsesAsManifest(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		minimal bool
	}{
		{"default", "", false},
		{"full coords", "acme/dev/tools/my-app", false},
		{"partial coords", "acme", false},
		{"minimal", "acme/dev/tools/my-app", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, stdout, _ := useFake(t, &fakeCA{})
			if err := runInit(cfg, c.target, c.minimal); err != nil {
				t.Fatalf("init: %v", err)
			}
			// manifest.Load expects a path with a relative README.md
			// alongside the file, since the template's example source is
			// ./README.md. Provide one so existence check passes.
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("placeholder"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "m.yaml")
			if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := manifest.Load(path); err != nil {
				t.Fatalf("manifest.Load(init output) failed: %v\noutput:\n%s", err, stdout.String())
			}
		})
	}
}
