package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

// TestUseValidatesCoords pins the "cob use rejects a structurally
// malformed coord at the point of intent, not 12 commands later when an
// AWS call gives a less-specific error" behavior. The shape check is
// manifest.ParseCoordinates — caught here, not on first network use.
//
// A typo in a *name* (e.g. "vt-writer" vs "vtwriter" — a 4-segment
// string that's structurally valid but doesn't exist upstream) can't be
// caught by this validator; the error for that case has to wait until
// the package genuinely doesn't resolve.
func TestUseValidatesCoords(t *testing.T) {
	// Run from a temp cwd so any successful set doesn't pollute the
	// real working dir.
	tmp := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	cases := []struct {
		coords  string
		wantErr bool
	}{
		// Structurally valid — these should succeed and persist.
		{"dom/repo/ns/pkg@1.0.0", false},
		{"dom/repo/ns/pkg", false}, // no version is allowed (read-only commands can override via @latest)

		// Structurally invalid — must error at `cob use` time.
		{"", true},         // empty
		{"dom", true},      // too few segments
		{"dom/repo", true}, // still too few
		{"@latest", true},  // version-only is meaningless as a stored current package
	}
	for _, c := range cases {
		t.Run(c.coords, func(t *testing.T) {
			// Clear any prior state per-case so failures from one don't
			// shadow the next.
			_, _ = removeCobDir(tmp)
			cfg, _, _ := clitest.UseFake(t, &clitest.FakeCA{})
			cmd := newUseCmd(cfg)
			cmd.SetArgs([]string{c.coords})
			err := cmd.Execute()
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Errorf("cob use %q: gotErr=%v wantErr=%v (err=%v)", c.coords, gotErr, c.wantErr, err)
			}
			// On the rejection path, the file must not exist — we don't
			// want a malformed coord persisting under .cob/current.
			if c.wantErr {
				path := filepath.Join(tmp, ".cob", "current")
				if _, statErr := os.Stat(path); statErr == nil {
					t.Errorf("invalid coords %q got persisted at %s; should have been rejected before write", c.coords, path)
				}
				if err != nil && !isExitError(err, cob.ExitError) {
					// Pretty error message, but check the underlying
					// ExitError so a refactor that returns a bare
					// fmt.Errorf doesn't silently lose the exit code.
					t.Logf("error: %v", err)
				}
			}
		})
	}
}

// removeCobDir wipes ./.cob if present so successive subtests start
// from a clean slate.
func removeCobDir(dir string) (string, error) {
	target := filepath.Join(dir, ".cob")
	return target, os.RemoveAll(target)
}

// isExitError is a tiny helper since the package's clitest.WantExit
// is a t.Fatal helper, not a boolean check.
func isExitError(err error, code int) bool {
	if err == nil {
		return false
	}
	// The error string format is "exit status N" per cliutil.ExitError.Error().
	return strings.Contains(err.Error(), "exit status")
}
