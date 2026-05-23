package output

import (
	"io"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jmurray2011/cob/internal/cob"
)

// liveRenderer drives a bubbletea program that paints a multi-row
// progress view in place. It implements the renderer interface by
// translating each event into a tea.Msg and dispatching to the running
// program.
//
// Lazy start: the bubbletea program is not launched until the first
// asset event arrives. Commands that never emit asset events (validate,
// log, ls, tree, …) leave the renderer dormant, and their Header /
// Plain / Summary writes go to stdout cleanly — without an empty TUI
// painting blank rows underneath and racing with each write.
//
// On TERM=dumb or similar degraded terminals, tea.Program falls back to
// minimal output rather than failing — the renderer therefore stays
// safe to install even when terminal capabilities are uncertain.
type liveRenderer struct {
	out io.Writer

	startOnce sync.Once
	program   *tea.Program // nil until ensureStarted runs

	doneOnce sync.Once
	done     chan struct{}
	runErr   error
}

// newLiveRenderer returns a dormant renderer. The bubbletea program
// starts lazily on the first asset event.
func newLiveRenderer(out io.Writer) *liveRenderer {
	return &liveRenderer{out: out}
}

// ensureStarted spins up the bubbletea program on demand. Idempotent;
// subsequent calls are no-ops. The program writes to lr.out and runs in
// its own goroutine. Mode-related options (no alt-screen, no signal
// handler) are documented near their flags below.
func (l *liveRenderer) ensureStarted() {
	l.startOnce.Do(func() {
		m := newLiveModel()
		l.program = tea.NewProgram(
			m,
			tea.WithOutput(l.out),
			// Inline (no WithAltScreen): operators want the final view
			// to remain in their scrollback after exit.
			tea.WithoutSignalHandler(), // cob's main wires its own ctx cancellation
		)
		l.done = make(chan struct{})
		go func() {
			defer close(l.done)
			_, l.runErr = l.program.Run()
		}()
	})
}

func (l *liveRenderer) AssetsExpected(count int, totalBytes int64) {
	l.ensureStarted()
	l.program.Send(assetsExpectedMsg{count: count, totalBytes: totalBytes})
}

func (l *liveRenderer) AssetStart(name, sourceURI string, size int64) {
	l.ensureStarted()
	l.program.Send(assetStartMsg{name: name, sourceURI: sourceURI, size: size})
}

func (l *liveRenderer) AssetProgress(name string, delta int64) {
	l.ensureStarted()
	l.program.Send(assetProgressMsg{name: name, delta: delta})
}

func (l *liveRenderer) AssetOK(r *cob.AssetResult, sourceURI string) {
	l.ensureStarted()
	l.program.Send(assetDoneMsg{
		name:       r.Name,
		sourceURI:  sourceURI,
		size:       r.Size,
		sha256:     r.SHA256,
		durationMs: r.DurationMs,
		method:     r.Method,
	})
}

func (l *liveRenderer) AssetFail(name, sourceURI string, err error) {
	l.ensureStarted()
	l.program.Send(assetFailMsg{name: name, sourceURI: sourceURI, err: err})
}

func (l *liveRenderer) AssetSkipped(name string) {
	l.ensureStarted()
	l.program.Send(assetSkipMsg{name: name})
}

// Close sends a Quit to the program (if it ever started) and waits for
// it to exit. Idempotent. A no-op when the renderer stayed dormant.
// Without the wait, subsequent terminal writes (the command's Summary
// line, the next shell prompt) could appear before bubbletea finishes
// its final View paint.
func (l *liveRenderer) Close() {
	l.doneOnce.Do(func() {
		if l.program == nil {
			return // dormant — nothing to tear down
		}
		l.program.Send(quitMsg{})
		<-l.done
	})
}
