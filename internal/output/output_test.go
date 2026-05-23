package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
)

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KB",
		1536:       "1.5 KB",
		1048576:    "1.0 MB",
		134217728:  "128.0 MB",
		1073741824: "1.0 GB",
		5368709120: "5.0 GB",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int64]string{
		0:     "0ms",
		1:     "1ms",
		999:   "999ms",
		1000:  "1.0s",
		1500:  "1.5s",
		90000: "90.0s",
	}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWarnSurfacesInJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := NewWithWriters(&stdout, &stderr, Mode{JSON: true})

	w.Warn("using COB_DOMAIN=acme-prod (overrides manifest domain)")

	// stderr carries it even in JSON mode (stderr != the stdout JSON stream).
	if !strings.Contains(stderr.String(), "COB_DOMAIN=acme-prod") {
		t.Errorf("warning missing from stderr: %q", stderr.String())
	}

	// and it is folded into the JSON CommandResult so a consumer sees it.
	if err := w.CommandResult(&cob.CommandResult{Command: "publish", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	var got cob.CommandResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "COB_DOMAIN=acme-prod") {
		t.Errorf("Warnings = %v, want the override notice", got.Warnings)
	}
}

func TestSetTerminalWidthForTestSwapsAndRestores(t *testing.T) {
	// Pin to a known width; both reads return it.
	restore := SetTerminalWidthForTest(120)
	if got := TerminalWidth(); got != 120 {
		t.Errorf("TerminalWidth after pin = %d, want 120", got)
	}
	if got := TerminalWidth(); got != 120 {
		t.Errorf("TerminalWidth must be stable across calls; got %d", got)
	}
	// Defer-style restore: subsequent calls hit the real fn again.
	restore()
	if got := TerminalWidth(); got == 120 {
		// The default reader returns whatever the host environment yields
		// (usually 80 in CI; rarely 120). A 120 here means restore didn't
		// fire — false positives are vanishingly unlikely.
		t.Errorf("TerminalWidth still 120 after restore — seam didn't reset")
	}
}

func TestQuietSuppressesChatter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := NewWithWriters(&stdout, &stderr, Mode{Quiet: true})

	w.Header("Publishing X")
	w.Summary("done")
	w.Plain("a note")
	if stdout.Len() != 0 {
		t.Errorf("--quiet must suppress header/summary/plain, got %q", stdout.String())
	}

	// Errors must still reach the user.
	w.Error("boom")
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("--quiet must not suppress errors, stderr = %q", stderr.String())
	}
}

// TestQuietPlusJSONStillEmitsResult pins the joint --quiet + --json
// contract:
//
//  1. stdout still gets the JSON CommandResult — the whole point of
//     --json is machine consumption, and a CI pipeline that passes
//     --quiet alongside doesn't want the result silently dropped.
//  2. Warnings still ride along inside that JSON (folded via Warnings[])
//     and still print to stderr — they're operationally important
//     (override notices, paginator caps, …) and stderr is separate from
//     the stdout JSON stream, so it can't corrupt machine parsing.
//  3. Errors still print to stderr unconditionally.
//
// This is the same behavior as --json alone today; --quiet adds no new
// suppression on top because --json already silences all human chatter
// on stdout. The test exists so a future refactor that special-cases
// the combo (e.g. dropping warnings when both flags are set) fails
// the build instead of regressing CI consumers silently.
func TestQuietPlusJSONStillEmitsResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := NewWithWriters(&stdout, &stderr, Mode{JSON: true, Quiet: true})

	w.Header("ignored on stdout")
	w.Plain("ignored on stdout")
	w.Summary("ignored on stdout")
	w.Warn("override notice")
	w.Error("boom")
	if err := w.CommandResult(&cob.CommandResult{Command: "publish", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	// stdout must be exactly the JSON CommandResult — nothing else.
	var got cob.CommandResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout should be parseable JSON when --json is set, got %q: %v", stdout.String(), err)
	}
	if got.Command != "publish" || got.Status != "ok" {
		t.Errorf("CommandResult round-trip wrong: %+v", got)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "override notice") {
		t.Errorf("warning should be folded into JSON Warnings[] even with --quiet, got %v", got.Warnings)
	}
	// stderr keeps both messages — stderr is the operator channel even
	// when stdout is locked down for the machine.
	if !strings.Contains(stderr.String(), "override notice") {
		t.Errorf("warning should still print to stderr with --quiet --json, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("error should still print to stderr with --quiet --json, got %q", stderr.String())
	}
}
