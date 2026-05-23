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
