package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/output"
)

// TestRenderVerifyMatchCompactWhenItFits is the regression for the
// "lines spill into next-line continuations" complaint: when the
// terminal is wide enough, the match row stays on one line +
// sha-continuation; when it's not, the URI drops to its own labeled
// line so wrapping never fragments mid-field.
func TestRenderVerifyMatchCompactWhenItFits(t *testing.T) {
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, output.Mode{})
	uri := "s3://b/short.bin"
	hash := strings.Repeat("a", 64)
	renderVerifyMatch(w, "short.bin", uri, "match(source)", hash, 12, 200)

	out := buf.String()
	// One-liner: name + URI + status on the header line.
	if !strings.Contains(out, "short.bin") || !strings.Contains(out, uri) || !strings.Contains(out, "match(source)") {
		t.Errorf("compact form should put name+uri+status on one line:\n%s", out)
	}
	// And the sha-continuation line follows.
	if !strings.Contains(out, "sha256") || !strings.Contains(out, hash) {
		t.Errorf("compact form should still include sha256 on its own line:\n%s", out)
	}
}

func TestRenderVerifyMatchExpandsWhenItDoesNotFit(t *testing.T) {
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, output.Mode{})
	// Long URI that would push the compact form past the terminal.
	longURI := "ca://" + strings.Repeat("x/", 60) + "asset.bin"
	renderVerifyMatch(w, "asset.bin", longURI, "match(source)", strings.Repeat("a", 64), 12, 80)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expanded form should be header + source + sha256 = 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	if strings.Contains(lines[0], longURI) {
		t.Errorf("header line should NOT carry the long URI on a narrow terminal; got %q", lines[0])
	}
	// Each detail line names its field — operators reading the row
	// shouldn't have to guess what a bare URI or bare hash represents.
	if !strings.Contains(buf.String(), "source") || !strings.Contains(buf.String(), "sha256") {
		t.Errorf("expanded form should label both detail lines:\n%s", buf.String())
	}
}

func TestRenderVerifyMismatchAlwaysExpandedWithFullHashes(t *testing.T) {
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, output.Mode{})
	leftHash := strings.Repeat("1", 64)
	rightHash := strings.Repeat("2", 64)
	renderVerifyMismatch(w, "asset.bin", 12,
		verifySide{label: "local", uri: "/path/to/asset.bin", size: 1024, hash: leftHash},
		verifySide{label: "published", uri: "dom/repo/ns/pkg@1.0.0", size: 2048, hash: rightHash},
	)
	out := buf.String()
	for _, want := range []string{
		"asset.bin", "mismatch",
		"local", "/path/to/asset.bin", leftHash,
		"published", "dom/repo/ns/pkg@1.0.0", rightHash,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mismatch output missing %q in:\n%s", want, out)
		}
	}
	// Each hash gets its own line — never embedded in a line that also
	// carries the size/URI. Without this, narrow terminals fragment
	// the hash mid-string.
	if !strings.Contains(out, "\n                 "+leftHash) ||
		!strings.Contains(out, "\n                 "+rightHash) {
		t.Errorf("each hash should be on its own indented continuation line:\n%s", out)
	}
}

func TestPickNameColumnWidth(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  int
	}{
		{"empty falls back to min", nil, 12},
		{"short names use min", []string{"a", "b"}, 12},
		{"sized to the longest", []string{"some-asset.bin", "x"}, 14},
		{"capped at max even when longer", []string{strings.Repeat("a", 60)}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickNameColumnWidth(c.input); got != c.want {
				t.Errorf("pickNameColumnWidth(%v) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}
