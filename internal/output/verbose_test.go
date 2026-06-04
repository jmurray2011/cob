package output

import (
	"bytes"
	"strings"
	"testing"
)

// TestVerbosef pins the --verbose sink: a leveled "verbose:" prefix on
// stderr only when verbose mode is on, never on stdout (so --json stays
// clean), and emitted even under --quiet (the operator opted in explicitly).
func TestVerbosef(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		var out, errOut bytes.Buffer
		w := NewWithWriters(&out, &errOut, Mode{})
		w.Verbosef("resolved %s", "x")
		if errOut.Len() != 0 || out.Len() != 0 {
			t.Errorf("verbose off should emit nothing; stdout=%q stderr=%q", out.String(), errOut.String())
		}
	})
	t.Run("on writes prefixed line to stderr only", func(t *testing.T) {
		var out, errOut bytes.Buffer
		w := NewWithWriters(&out, &errOut, Mode{Verbose: true})
		w.Verbosef("aws %s %s", "ListAssets", "acme/dev/tools/app@1.0.0")
		if out.Len() != 0 {
			t.Errorf("verbose must not touch stdout, got %q", out.String())
		}
		got := errOut.String()
		if !strings.HasPrefix(got, "verbose: ") {
			t.Errorf("missing leveled prefix: %q", got)
		}
		if !strings.Contains(got, "aws ListAssets acme/dev/tools/app@1.0.0") {
			t.Errorf("missing message: %q", got)
		}
	})
	t.Run("emitted even under --quiet", func(t *testing.T) {
		var out, errOut bytes.Buffer
		w := NewWithWriters(&out, &errOut, Mode{Verbose: true, Quiet: true})
		w.Verbosef("still here")
		if !strings.Contains(errOut.String(), "still here") {
			t.Errorf("--verbose should survive --quiet (explicit opt-in); got %q", errOut.String())
		}
	})
}
