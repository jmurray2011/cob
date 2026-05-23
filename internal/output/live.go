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
// program. The program runs in its own goroutine; Close sends a quit
// message and waits for it to exit so subsequent terminal output isn't
// interleaved with a still-painting TUI.
//
// On TERM=dumb or similar degraded terminals, tea.Program falls back to
// minimal output rather than failing — the renderer therefore stays
// safe to install even when terminal capabilities are uncertain.
type liveRenderer struct {
	program *tea.Program

	doneOnce sync.Once
	done     chan struct{}
	runErr   error
}

// newLiveRenderer builds a bubbletea program writing to out and starts it
// in the background. The Mode argument is informational only — by the
// time we're constructing this, the picker has already decided it's
// appropriate.
func newLiveRenderer(out io.Writer) *liveRenderer {
	m := newLiveModel()
	prog := tea.NewProgram(
		m,
		tea.WithOutput(out),
		// We deliberately do NOT use WithAltScreen — operators want the
		// run to remain in their scrollback after exit; the inline mode
		// scrolls naturally with the rest of the shell.
		tea.WithoutSignalHandler(), // cob's main wires its own ctx cancellation
	)
	lr := &liveRenderer{program: prog, done: make(chan struct{})}
	go func() {
		defer close(lr.done)
		_, lr.runErr = prog.Run()
	}()
	return lr
}

func (l *liveRenderer) AssetsExpected(count int, totalBytes int64) {
	l.program.Send(assetsExpectedMsg{count: count, totalBytes: totalBytes})
}

func (l *liveRenderer) AssetStart(name, sourceURI string, size int64) {
	l.program.Send(assetStartMsg{name: name, sourceURI: sourceURI, size: size})
}

func (l *liveRenderer) AssetProgress(name string, delta int64) {
	l.program.Send(assetProgressMsg{name: name, delta: delta})
}

func (l *liveRenderer) AssetOK(r *cob.AssetResult, sourceURI string) {
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
	l.program.Send(assetFailMsg{name: name, sourceURI: sourceURI, err: err})
}

func (l *liveRenderer) AssetSkipped(name string) {
	l.program.Send(assetSkipMsg{name: name})
}

// Close sends a Quit to the program and waits for it to exit. Idempotent:
// repeat calls return immediately once the program is gone. Without this
// wait, subsequent terminal writes (the command's Summary line, the next
// shell prompt) could appear before bubbletea has finished its final
// View paint.
func (l *liveRenderer) Close() {
	l.doneOnce.Do(func() {
		l.program.Send(quitMsg{})
		<-l.done
	})
}
