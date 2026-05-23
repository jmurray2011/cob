package output

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// TestLiveTeaFinalPaintSnapshot runs the live model through the actual
// bubbletea program (not just calls to Update) and snapshots the final
// View. This is the only test that exercises the rendering pipeline
// end-to-end — Init, the ticker loop, the WindowSizeMsg flow, View
// after Quit — so it's where layout regressions surface (column drift,
// box-drawing changes, accidental newlines).
//
// teatest is in `x/exp` so the API can move; the cost of a bumpy
// upgrade is small relative to losing this guard against silent visual
// changes.
func TestLiveTeaFinalPaintSnapshot(t *testing.T) {
	m := newLiveModel(&atomic.Bool{}, nil)
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 30))

	// Drive a representative scenario — one match, one failure, one
	// skip. Three states cover every label/glyph the renderer can
	// emit, so the snapshot fails on any of them changing.
	tm.Send(assetsExpectedMsg{count: 3, totalBytes: 100})
	tm.Send(assetStartMsg{name: "alpha.bin", size: 50})
	tm.Send(assetStartMsg{name: "beta.bin", size: 30})
	tm.Send(assetStartMsg{name: "gamma.bin", size: 20})
	tm.Send(assetProgressMsg{name: "alpha.bin", delta: 50})
	tm.Send(assetDoneMsg{name: "alpha.bin", size: 50, durationMs: 100, method: "match(source)"})
	tm.Send(assetFailMsg{name: "beta.bin", err: errSnapshot{}})
	tm.Send(assetSkipMsg{name: "gamma.bin"})

	// Give the program a moment to drain the messages before we ask
	// for the final view. WaitFor polls until the output stabilizes.
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		// All three rows have to be reflected before quitting, or the
		// snapshot captures a half-painted frame.
		return strings.Contains(string(bts), "alpha.bin") &&
			strings.Contains(string(bts), "beta.bin") &&
			strings.Contains(string(bts), "gamma.bin")
	}, teatest.WithDuration(2*time.Second))

	tm.Send(quitMsg{})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))

	// Render the final-model View directly rather than reading from the
	// program's output stream. bubbletea's exit sequence emits cursor-
	// reposition + clear-line escapes that obliterate the captured
	// bytes; the model's last View is the stable representation of
	// what was on screen at quit time. Strip ANSI color sequences so
	// assertions don't depend on the host's TERM/NO_COLOR/palette.
	plain := stripANSI(final.View())

	// Each row's glyph + name + a state cue must show in the final
	// paint. If any of these disappears the renderer broke the
	// row-by-row contract.
	for _, want := range []string{
		"✓ alpha.bin",                // done row keeps its check
		"alpha.bin", "match(source)", // method label preserved
		"✗ beta.bin",   // failed row keeps its cross
		"snapshot-err", // failure carries the error text
		"⊘ gamma.bin",  // skipped row keeps its symbol
		"skipped",      // and its label
		"Total:",       // summary line lands at the bottom
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("final paint missing %q\n---\n%s\n---", want, plain)
		}
	}
}

// stripANSI removes ESC[...m and ESC[...J/H/K control sequences so the
// snapshot assertions don't depend on the host terminal's color
// capabilities (NO_COLOR, palette, TERM=dumb all change the bytes
// without changing the layout).
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) {
				c := s[j]
				j++
				if (c >= 0x40 && c <= 0x7e) || c == ';' && j == len(s) {
					break
				}
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// errSnapshot is a stable error for the snapshot — using errors.New
// would risk a pointer-formatted message; this keeps the asserted
// substring "snapshot-err" deterministic.
type errSnapshot struct{}

func (errSnapshot) Error() string { return "snapshot-err" }

var _ tea.Msg = quitMsg{} // ensure quitMsg stays a valid Msg type
