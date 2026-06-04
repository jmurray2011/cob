package output

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// TestTruncateNameUnicode pins that truncation respects rune boundaries and
// display width. The name column measures width in cells (lipgloss.Width)
// but truncation used to byte-slice, cutting a multibyte rune in half and
// emitting invalid UTF-8 for CJK/emoji asset names.
func TestTruncateNameUnicode(t *testing.T) {
	// Each 世 renders in 2 cells and is 3 bytes in UTF-8. width=9 forces the
	// byte cut to land at byte 8 — mid-rune — which is exactly what the old
	// byte-slice got wrong.
	name := strings.Repeat("世", 20) // width 40, 60 bytes
	got := truncateName(name, 9)

	if !utf8.ValidString(got) {
		t.Errorf("truncateName produced invalid UTF-8: %q (% x)", got, got)
	}
	if w := lipgloss.Width(got); w > 9 {
		t.Errorf("truncated width = %d, must be <= 9: %q", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated name should end with the ellipsis: %q", got)
	}
}

// TestTruncateNameASCIIUnchanged keeps the common path byte-identical to the
// old behavior: width-1 chars plus the ellipsis.
func TestTruncateNameASCIIUnchanged(t *testing.T) {
	if got, want := truncateName("application-server.bin", 10), "applicati…"; got != want {
		t.Errorf("truncateName ASCII = %q, want %q", got, want)
	}
	if got := truncateName("short", 10); got != "short" {
		t.Errorf("a name within width must pass through unchanged, got %q", got)
	}
}
