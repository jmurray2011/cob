package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/output"
)

// TestRenderVerifyMatchTerseDefault locks the contract that motivated
// switching the default: a routine match emits one line — glyph,
// name, size, method — and nothing else. No URI, no SHA. 12 such
// rows next to 3 mismatches should leave the failures unmissable
// instead of buried in audit detail nobody asked for.
func TestRenderVerifyMatchTerseDefault(t *testing.T) {
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, output.Mode{})
	uri := "s3://b/short.bin"
	hash := strings.Repeat("a", 64)
	renderVerifyMatch(w, "short.bin", uri, "match(source)", hash, 1024, 12, 200, false)

	out := strings.TrimSpace(buf.String())
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("terse match should be exactly one line; got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, "short.bin") || !strings.Contains(out, "1.0 KB") || !strings.Contains(out, "match(source)") {
		t.Errorf("terse match should carry name + size + method:\n%s", out)
	}
	// URI and hash are deliberately absent on the terse path.
	if strings.Contains(out, uri) {
		t.Errorf("terse match must NOT include the URI (use --verbose):\n%s", out)
	}
	if strings.Contains(out, hash) {
		t.Errorf("terse match must NOT include the full hash (use --verbose):\n%s", out)
	}
}

// TestRenderVerifyMatchVerboseAddsAuditLines confirms --verbose
// puts the URI and full SHA-256 back as labeled continuation lines.
func TestRenderVerifyMatchVerboseAddsAuditLines(t *testing.T) {
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, output.Mode{})
	uri := "s3://b/short.bin"
	hash := strings.Repeat("a", 64)
	renderVerifyMatch(w, "short.bin", uri, "match(source)", hash, 1024, 12, 200, true)

	out := buf.String()
	for _, want := range []string{"short.bin", "1.0 KB", "match(source)", "source", uri, "sha256", hash} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose match should include %q:\n%s", want, out)
		}
	}
	// Three lines exactly: header + source + sha256.
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 3 {
		t.Errorf("verbose match should be 3 lines; got %d:\n%s", n, out)
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
