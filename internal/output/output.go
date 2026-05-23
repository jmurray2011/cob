package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/charmbracelet/x/term"

	"github.com/jmurray2011/cob/internal/cob"
)

// Writer is the per-command output sink. Its public surface stays
// stable across modes — callers emit semantic events (Header,
// AssetStart, AssetProgress, …); the renderer chosen at construction
// (silent / stream / live) decides how to draw them. JSON-mode output
// goes through CommandResult / JSON only; non-JSON output goes through
// the renderer for the asset stream and through the printf-style
// methods for headers, summaries, and warnings.
type Writer struct {
	out      io.Writer
	errOut   io.Writer
	mode     Mode
	renderer renderer

	mu       sync.Mutex
	warnings []string

	closeOnce sync.Once
}

// New creates a Writer for the given mode and auto-selects a renderer
// based on terminal detection. JSON or Quiet → silent; interactive TTY
// (and NoTUI not set, COB_TUI not "0") → live; otherwise → stream.
func New(mode Mode) *Writer {
	w := &Writer{out: os.Stdout, errOut: os.Stderr, mode: mode}
	w.renderer = pickRenderer(mode, os.Stdout, IsTerminal(os.Stdout))
	return w
}

// NewWithWriters is the explicit-destination constructor used by tests.
// isTTY is forced false so the renderer is deterministic regardless of
// the host test environment.
func NewWithWriters(out, errOut io.Writer, mode Mode) *Writer {
	w := &Writer{out: out, errOut: errOut, mode: mode}
	w.renderer = pickRenderer(mode, out, false)
	return w
}

// pickRenderer applies the decision tree described on Mode. Note that
// COB_TUI=0 is honored by the CLI layer (it maps onto Mode.NoTUI in
// applyEnvFallbacks); the output package only sees the resolved Mode
// so it can stay free of env-var knowledge.
func pickRenderer(mode Mode, out io.Writer, isTTY bool) renderer {
	if mode.JSON || mode.Quiet {
		return silentRenderer{}
	}
	if isTTY && !mode.NoTUI {
		return newLiveRenderer(out)
	}
	return &streamRenderer{out: out}
}

// Close tears down the renderer (waits for any live TUI program to
// finish painting). Safe to call multiple times; safe to call before
// any asset-stream method was invoked.
func (w *Writer) Close() {
	w.closeOnce.Do(func() {
		if w.renderer != nil {
			w.renderer.Close()
		}
	})
}

// SetInterrupt registers a callback fired when the user hits Ctrl-C
// inside the live TUI. Bubbletea's raw mode swallows the kernel's
// translation of Ctrl-C into SIGINT, so the OS-level signal.NotifyContext
// in main never fires while the TUI is up — this is the only mechanism
// that reaches in-flight pull/publish/promote goroutines. Wire it to
// your context's cancel func before kicking off transfers.
//
// No-op on non-live renderers (silent / stream) — they don't need the
// rescue plumbing because they don't put the terminal in raw mode.
func (w *Writer) SetInterrupt(fn func()) {
	if l, ok := w.renderer.(*liveRenderer); ok {
		l.SetInterrupt(fn)
	}
}

// Interrupted reports whether the user hit Ctrl-C inside the live TUI.
// Only meaningful in live mode; returns false otherwise. Call after
// Close to decide whether to surface a distinct exit status or footer.
func (w *Writer) Interrupted() bool {
	if l, ok := w.renderer.(*liveRenderer); ok {
		return l.Interrupted()
	}
	return false
}

// Stdout returns the writer's stdout sink, for command output that is
// neither an asset line, a table, nor a CommandResult — resolve's bare
// version string and manifest's YAML document. Routing through here (rather
// than os.Stdout directly) keeps that output capturable in tests.
func (w *Writer) Stdout() io.Writer { return w.out }

// Aborted reports a user-declined confirmation. Stderr gets a one-line
// human message; --json gets a parseable CommandResult so a CI consumer
// can tell "the user declined" from a silent exit-0 success.
func (w *Writer) Aborted(command string) {
	if w.mode.JSON {
		w.CommandResult(&cob.CommandResult{Command: command, Status: "aborted"})
		return
	}
	fmt.Fprintln(w.errOut, "Aborted.")
}

// CommandResult writes the final result of a command. Any warnings emitted
// during the command are folded in first, so a --json consumer (which never
// sees the stderr warning lines) still gets them.
func (w *Writer) CommandResult(result *cob.CommandResult) error {
	if result.Warnings == nil {
		result.Warnings = w.warnings
	}
	if w.mode.JSON {
		enc := json.NewEncoder(w.out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return nil
}

// ErrorResult emits a minimal JSON error result (for --json mode) and
// writes the error to stderr. Use for early failures before any assets
// are processed, so CI pipelines always get parseable JSON on stdout.
func (w *Writer) ErrorResult(command, errMsg string) {
	w.Error("%s", errMsg)
	if w.mode.JSON {
		w.CommandResult(&cob.CommandResult{
			Command: command,
			Status:  "error",
			Error:   errMsg,
		})
	}
}

// Header prints the initial command header (non-JSON, non-quiet mode).
func (w *Writer) Header(format string, args ...any) {
	if !w.mode.JSON && !w.mode.Quiet {
		fmt.Fprintf(w.out, format+"\n", args...)
		fmt.Fprintln(w.out)
	}
}

// AssetsExpected announces the size of the upcoming asset stream so the
// live renderer can scope its total/percentage display. Stream and
// silent renderers ignore it. Pass totalBytes=0 if sizes aren't known up
// front (publish/promote resolve sources lazily).
func (w *Writer) AssetsExpected(count int, totalBytes int64) {
	w.renderer.AssetsExpected(count, totalBytes)
}

// AssetStart announces an asset transfer is beginning.
func (w *Writer) AssetStart(name, sourceURI string, size int64) {
	w.renderer.AssetStart(name, sourceURI, size)
}

// AssetProgress reports a byte-count delta for the named asset. Safe to
// call from concurrent goroutines — renderers serialize internally.
// Designed to be plugged in as the Progress field on cob.Puller /
// Publisher / Promoter.
func (w *Writer) AssetProgress(name string, delta int64) {
	w.renderer.AssetProgress(name, delta)
}

// AssetOK marks a successful asset transfer.
func (w *Writer) AssetOK(r *cob.AssetResult, sourceURI string) {
	w.renderer.AssetOK(r, sourceURI)
}

// AssetFail marks a failed asset transfer.
func (w *Writer) AssetFail(name, sourceURI string, err error) {
	w.renderer.AssetFail(name, sourceURI, err)
}

// AssetSkipped marks a skipped asset.
func (w *Writer) AssetSkipped(name string) {
	w.renderer.AssetSkipped(name)
}

// Plain prints a formatted line to stdout (non-JSON mode only). For ad-hoc
// human output that is neither an asset transfer line nor a summary.
func (w *Writer) Plain(format string, args ...any) {
	if w.mode.JSON || w.mode.Quiet {
		return
	}
	fmt.Fprintf(w.out, format+"\n", args...)
}

// Summary prints the final summary line.
func (w *Writer) Summary(format string, args ...any) {
	if !w.mode.JSON && !w.mode.Quiet {
		fmt.Fprintln(w.out)
		fmt.Fprintf(w.out, format+"\n", args...)
	}
}

// Notice writes an informational line to stderr — no prefix, no JSON
// fold-in. For headers that should be visible to an interactive operator
// but must NOT touch stdout, because stdout is being captured by a
// script (e.g. `VERSION=$(cob resolve ...)`). Plain/Header would land on
// stdout; Warn would prepend "Warning:". Notice is the missing middle.
//
// Suppressed in --quiet mode so a `--quiet --json` script gets nothing
// but the structured output it asked for, on the right channel.
func (w *Writer) Notice(format string, args ...any) {
	if w.mode.Quiet {
		return
	}
	fmt.Fprintf(w.errOut, format+"\n", args...)
}

// Warn records a warning and writes it to stderr. It fires in every mode,
// including --json: stderr is separate from the stdout JSON stream, so it
// cannot corrupt machine-readable output, and a warning silently dropped in
// CI (e.g. a COB_DOMAIN override retargeting a publish) is a real footgun.
// The warning is also retained for CommandResult to surface in JSON.
func (w *Writer) Warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	w.warnings = append(w.warnings, msg)
	fmt.Fprintln(w.errOut, "Warning: "+msg)
}

// Error writes to stderr.
func (w *Writer) Error(format string, args ...any) {
	fmt.Fprintf(w.errOut, "Error: "+format+"\n", args...)
}

// JSON writes an arbitrary value as indented JSON to stdout.
// Returns true if JSON mode is active (and the value was written),
// false if the caller should fall through to human output.
func (w *Writer) JSON(v any) bool {
	if !w.mode.JSON {
		return false
	}
	enc := json.NewEncoder(w.out)
	enc.SetIndent("", "  ")
	enc.Encode(v)
	return true
}

// Table prints tabulated output for ls commands.
func (w *Writer) Table(headers []string, rows [][]string) {
	if w.mode.JSON {
		return
	}
	tw := tabwriter.NewWriter(w.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
}

// FormatSize returns a human-readable size string.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// FormatDuration returns a human-readable duration string.
func FormatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// IsTerminal reports whether f is a character device (an interactive
// terminal). Shared so callers needn't re-implement the Stat dance.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// TerminalWidth returns the current stdout column count for adaptive
// rendering (diff's row-format picker, future help text wrapping, etc.).
// Falls back to $COLUMNS, then to 80, so non-TTY callers get a sensible
// default without erroring. Cheap to call repeatedly.
//
// Indirected through the terminalWidthFn package var so a test can pin
// a width and exercise width-sensitive rendering paths deterministically
// — the real implementation reads os.Stdout.Fd() which can't be spoofed
// from outside the process.
func TerminalWidth() int {
	return terminalWidthFn()
}

// terminalWidthFn is the test seam behind TerminalWidth. Swap it via
// SetTerminalWidthForTest (defer-restored) inside a test; production
// stays on defaultTerminalWidth.
var terminalWidthFn = defaultTerminalWidth

func defaultTerminalWidth() int {
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	if env := os.Getenv("COLUMNS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

// SetTerminalWidthForTest pins TerminalWidth to width for the duration of
// the calling test. Returns the restore func the test must defer — same
// shape every "swap a package var" test seam in this codebase uses, so a
// reader can spot the pattern at a glance.
//
// Intended only for tests; calling it from production code would silently
// break adaptive rendering on every other goroutine.
func SetTerminalWidthForTest(width int) (restore func()) {
	prev := terminalWidthFn
	terminalWidthFn = func() int { return width }
	return func() { terminalWidthFn = prev }
}
