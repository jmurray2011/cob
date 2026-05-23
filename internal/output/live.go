package output

import (
	"io"
	"sync"
	"sync/atomic"

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
// Ctrl-C: bubbletea's raw mode swallows the kernel's translation of
// Ctrl-C into SIGINT, so the OS-level signal.NotifyContext in main
// never fires while the TUI is up. The model catches the KeyMsg and
// invokes onInterrupt (set by Writer.SetInterrupt), which is wired by
// the runXxx caller to its context's cancel func. The pull/publish/
// promote goroutines see ctx.Err() and abort the in-flight AWS calls.
//
// On TERM=dumb or similar degraded terminals, tea.Program falls back to
// minimal output rather than failing — the renderer therefore stays
// safe to install even when terminal capabilities are uncertain.
type liveRenderer struct {
	out io.Writer

	// interrupted is shared with the bubbletea model. The model sets it
	// in Update on Ctrl-C; the goroutine that ran the program reads it
	// after Run() returns to distinguish "user quit" from "command
	// finished normally".
	interrupted *atomic.Bool
	onInterrupt func()
	mu          sync.Mutex // guards onInterrupt against concurrent SetInterrupt

	startOnce sync.Once
	program   *tea.Program // nil until ensureStarted runs

	doneOnce sync.Once
	done     chan struct{}
	runErr   error
}

// newLiveRenderer returns a dormant renderer. The bubbletea program
// starts lazily on the first asset event.
func newLiveRenderer(out io.Writer) *liveRenderer {
	return &liveRenderer{out: out, interrupted: &atomic.Bool{}}
}

// SetInterrupt registers a callback the renderer invokes when the user
// hits Ctrl-C inside the live TUI. Typical wiring is the runXxx's
// context.WithCancel cancel func — calling it abort-cancels every AWS
// call in flight (the SDK respects ctx). Safe to call before or after
// ensureStarted; not safe to call concurrently with itself.
func (l *liveRenderer) SetInterrupt(fn func()) {
	l.mu.Lock()
	l.onInterrupt = fn
	l.mu.Unlock()
}

// ensureStarted spins up the bubbletea program on demand. Idempotent;
// subsequent calls are no-ops. The program writes to lr.out and runs in
// its own goroutine.
func (l *liveRenderer) ensureStarted() {
	l.startOnce.Do(func() {
		l.mu.Lock()
		onInt := l.onInterrupt
		l.mu.Unlock()
		m := newLiveModel(l.interrupted, onInt)
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
	l.send(assetsExpectedMsg{count: count, totalBytes: totalBytes})
}

func (l *liveRenderer) AssetStart(name, sourceURI string, size int64) {
	l.ensureStarted()
	l.send(assetStartMsg{name: name, sourceURI: sourceURI, size: size})
}

func (l *liveRenderer) AssetProgress(name string, delta int64) {
	l.ensureStarted()
	l.send(assetProgressMsg{name: name, delta: delta})
}

func (l *liveRenderer) AssetOK(r *cob.AssetResult, sourceURI string) {
	l.ensureStarted()
	l.send(assetDoneMsg{
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
	l.send(assetFailMsg{name: name, sourceURI: sourceURI, err: err})
}

func (l *liveRenderer) AssetSkipped(name string) {
	l.ensureStarted()
	l.send(assetSkipMsg{name: name})
}

// send dispatches a message to the program, guarded against the race
// where the program has already exited (user hit Ctrl-C, or Close
// fired) and asset goroutines are still emitting final-state events.
// Bubbletea's Send after Quit is documented as safe but we wrap with
// recover for defense in depth.
func (l *liveRenderer) send(msg tea.Msg) {
	select {
	case <-l.done:
		return // program exited; further messages are no-ops
	default:
	}
	defer func() { _ = recover() }()
	l.program.Send(msg)
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
		l.send(quitMsg{})
		<-l.done
	})
}

// Interrupted reports whether the user hit Ctrl-C inside the live TUI.
// Callers can check this after Close to decide whether to exit with a
// distinct status code or print a "(canceled)" footer.
func (l *liveRenderer) Interrupted() bool {
	return l.interrupted.Load()
}
