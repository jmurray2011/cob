package output

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestLiveModelUpdateProducesRowsForEachEvent exercises Update directly
// (no bubbletea program needed) to verify each event kind populates the
// model state correctly. Bubbletea's runtime is mostly an event loop
// around Update; testing Update in isolation gives most of the coverage
// at a fraction of the complexity.
func TestLiveModelUpdateProducesRowsForEachEvent(t *testing.T) {
	m := newLiveModel(nil, nil)
	var im tea.Model = m

	send := func(msg tea.Msg) {
		im, _ = im.Update(msg)
	}

	send(assetsExpectedMsg{count: 3, totalBytes: 1000})
	send(assetStartMsg{name: "alpha.bin", size: 100})
	send(assetStartMsg{name: "beta.bin", size: 500})
	send(assetProgressMsg{name: "alpha.bin", delta: 50})
	send(assetProgressMsg{name: "beta.bin", delta: 250})
	send(assetDoneMsg{name: "alpha.bin", size: 100, durationMs: 320, method: "spilled"})
	send(assetFailMsg{name: "beta.bin", err: errors.New("nope")})
	send(assetSkipMsg{name: "gamma.bin"})

	got := im.(liveModel)
	if got.expectedCount != 3 || got.expectedBytes != 1000 {
		t.Errorf("expected = (%d, %d), want (3, 1000)", got.expectedCount, got.expectedBytes)
	}
	if len(got.rows) != 3 {
		t.Fatalf("rows = %d, want 3 (alpha, beta, gamma)", len(got.rows))
	}
	// alpha: done — bytes reconciled up to size.
	if alpha := got.byName["alpha.bin"]; alpha.state != stateDone || alpha.bytes != 100 {
		t.Errorf("alpha row wrong: %+v", alpha)
	}
	// beta: failed with err.
	if beta := got.byName["beta.bin"]; beta.state != stateFailed || beta.err == nil {
		t.Errorf("beta row wrong: %+v", beta)
	}
	// gamma: synthesized from skip event (no Start arrived).
	if gamma := got.byName["gamma.bin"]; gamma.state != stateSkipped {
		t.Errorf("gamma row wrong: %+v", gamma)
	}
}

// TestLiveModelViewRendersTerminalStates exercises the View() path on
// rows of every state — done, failed, skipped, active, queued — and
// asserts the rendered string includes a recognizable marker for each.
// Box-drawing / alignment regressions surface here.
func TestLiveModelViewRendersTerminalStates(t *testing.T) {
	m := newLiveModel(nil, nil)
	m.width = 100
	var im tea.Model = m
	send := func(msg tea.Msg) { im, _ = im.Update(msg) }

	send(assetStartMsg{name: "queued.bin", size: 10}) // will stay queued
	send(assetStartMsg{name: "active.bin", size: 1000})
	send(assetProgressMsg{name: "active.bin", delta: 500})
	send(assetStartMsg{name: "done.bin", size: 7})
	send(assetDoneMsg{name: "done.bin", size: 7, durationMs: 50, method: "match(source)"})
	send(assetStartMsg{name: "failed.bin", size: 1})
	send(assetFailMsg{name: "failed.bin", err: errors.New("boom")})
	send(assetSkipMsg{name: "skipped.bin"})

	// queued.bin needs to stay queued (no start kicked it to active in
	// this test's wiring) — reset its state manually.
	im.(liveModel).byName["queued.bin"].state = stateQueued

	out := im.(liveModel).View()
	for _, want := range []string{
		"done.bin", "match(source)", // done row carries method
		"active.bin", // active row name
		"failed.bin", // failed row name
		"boom",       // failed row carries error
		"skipped.bin", "skipped",
		"Total:", // summary line
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View missing %q in:\n%s", want, out)
		}
	}
}

// TestLiveModelCtrlCFiresInterruptAndQuits is the regression for the
// stuck-during-pull bug: bubbletea puts the terminal in raw mode (ISIG
// off), so the kernel never translates Ctrl-C into SIGINT. The model
// has to catch the KeyMsg, set the shared interrupted flag (so the
// renderer's post-Run path knows what happened), fire the registered
// cancel callback (so in-flight AWS calls abort immediately), and
// return tea.Quit (so bubbletea tears down the TUI). Skipping any of
// those leaves the user stuck.
func TestLiveModelCtrlCFiresInterruptAndQuits(t *testing.T) {
	interrupted := &atomic.Bool{}
	var canceled atomic.Bool
	m := newLiveModel(interrupted, func() { canceled.Store(true) })

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if !interrupted.Load() {
		t.Error("interrupted flag must be set on Ctrl-C")
	}
	if !canceled.Load() {
		t.Error("onInterrupt callback must fire on Ctrl-C")
	}
	if cmd == nil {
		t.Fatal("Ctrl-C must return tea.Quit")
	}
	// tea.Quit is a function; calling it returns a tea.QuitMsg.
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", cmd())
	}
}

// TestLiveModelOtherKeysIgnored confirms that the TUI is
// non-interactive — random keypresses do nothing. Without this guard a
// stray 'q' could quit a long-running pull mid-transfer.
func TestLiveModelOtherKeysIgnored(t *testing.T) {
	interrupted := &atomic.Bool{}
	var canceled atomic.Bool
	m := newLiveModel(interrupted, func() { canceled.Store(true) })

	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyEnter, tea.KeySpace} {
		_, cmd := m.Update(tea.KeyMsg{Type: key})
		if cmd != nil {
			t.Errorf("key %v should be a no-op, got cmd %v", key, cmd)
		}
	}
	// And a regular rune.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd != nil {
		t.Error("'q' should be a no-op (the TUI is non-interactive)")
	}
	if interrupted.Load() || canceled.Load() {
		t.Error("non-Ctrl-C keys must not trip the interrupt path")
	}
}

// TestOrderRowsSettlingOrder confirms the visual stability invariant:
// done/skipped/failed first, then active, then queued. Without this
// the view would jitter as in-flight rows finished out of arrival order.
func TestOrderRowsSettlingOrder(t *testing.T) {
	rows := []*assetRow{
		{name: "q1", state: stateQueued},
		{name: "a1", state: stateActive},
		{name: "d1", state: stateDone},
		{name: "a2", state: stateActive},
		{name: "f1", state: stateFailed},
		{name: "s1", state: stateSkipped},
		{name: "q2", state: stateQueued},
	}
	got := orderRows(rows)
	names := make([]string, len(got))
	for i, r := range got {
		names[i] = r.name
	}
	want := []string{"d1", "f1", "s1", "a1", "a2", "q1", "q2"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("orderRows[%d] = %q, want %q (full: %v)", i, names[i], w, names)
		}
	}
}
