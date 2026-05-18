package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jmurray2011/cob/pkg/cob"
)

// Writer handles formatted output for cob commands.
type Writer struct {
	out    io.Writer
	errOut io.Writer
	json   bool
	isTTY  bool
}

// New creates a Writer. If jsonMode is true, output is JSON.
// Otherwise it auto-detects TTY for human-friendly output.
func New(jsonMode bool) *Writer {
	w := NewWithWriters(os.Stdout, os.Stderr, jsonMode)
	w.isTTY = isTerminal(os.Stdout)
	return w
}

// NewWithWriters builds a Writer with explicit destinations. Used by tests
// to capture stdout/stderr; isTTY is forced false so formatting is
// deterministic regardless of the test environment.
func NewWithWriters(out, errOut io.Writer, jsonMode bool) *Writer {
	return &Writer{
		out:    out,
		errOut: errOut,
		json:   jsonMode,
		isTTY:  false,
	}
}

// CommandResult writes the final result of a command.
func (w *Writer) CommandResult(result *cob.CommandResult) error {
	if w.json {
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
	if w.json {
		w.CommandResult(&cob.CommandResult{
			Command: command,
			Status:  "error",
			Error:   errMsg,
		})
	}
}

// Header prints the initial command header (non-JSON mode).
func (w *Writer) Header(format string, args ...any) {
	if !w.json {
		fmt.Fprintf(w.out, format+"\n", args...)
		fmt.Fprintln(w.out)
	}
}

// AssetStart prints a "starting" status line before a transfer begins.
// Only emitted in interactive (TTY) non-JSON mode so logs and pipes stay clean.
// sourceURI is shown when known (publish); pass "" for pull where there is no
// source URI to display. size is the known content size (or 0 if unknown).
func (w *Writer) AssetStart(name, sourceURI string, size int64) {
	if w.json || !w.isTTY {
		return
	}
	line := "  .. " + name
	if sourceURI != "" {
		line += "  <-  " + sourceURI
	}
	if size > 0 {
		line += "  (" + FormatSize(size) + ")"
	}
	fmt.Fprintln(w.out, line)
	w.flush()
}

// AssetOK prints a successful asset transfer line.
func (w *Writer) AssetOK(r *cob.AssetResult, sourceURI string) {
	if w.json {
		return
	}
	sizeStr := FormatSize(r.Size)
	durStr := FormatDuration(r.DurationMs)
	if w.isTTY {
		line := "  OK " + r.Name
		if sourceURI != "" {
			line += "  <-  " + sourceURI
		}
		line += "  (" + sizeStr + ") " + durStr + " " + r.Method
		fmt.Fprintln(w.out, line)
	} else {
		fmt.Fprintf(w.out, "OK %s (%s) %s\n", r.Name, sizeStr, durStr)
	}
	w.flush()
}

// AssetFail prints a failed asset line.
func (w *Writer) AssetFail(name, sourceURI string, err error) {
	if w.json {
		return
	}
	if w.isTTY {
		line := "  FAIL " + name
		if sourceURI != "" {
			line += "  <-  " + sourceURI
		}
		line += "  " + err.Error()
		fmt.Fprintln(w.out, line)
	} else {
		fmt.Fprintf(w.out, "FAIL %s %s\n", name, err)
	}
	w.flush()
}

// AssetSkipped prints a skipped asset line.
func (w *Writer) AssetSkipped(name string) {
	if w.json {
		return
	}
	if w.isTTY {
		fmt.Fprintf(w.out, "  -- %s  (skipped)\n", name)
	} else {
		fmt.Fprintf(w.out, "SKIP %s\n", name)
	}
	w.flush()
}

// flush is a best-effort fsync on the underlying stdout. Some terminal
// integrations (notably WSL piped through IDE terminals) otherwise hold
// output until the process exits, which makes live progress useless.
func (w *Writer) flush() {
	if f, ok := w.out.(*os.File); ok {
		_ = f.Sync()
	}
}

// Plain prints a formatted line to stdout (non-JSON mode only). For ad-hoc
// human output that is neither an asset transfer line nor a summary.
func (w *Writer) Plain(format string, args ...any) {
	if w.json {
		return
	}
	fmt.Fprintf(w.out, format+"\n", args...)
	w.flush()
}

// Summary prints the final summary line.
func (w *Writer) Summary(format string, args ...any) {
	if !w.json {
		fmt.Fprintln(w.out)
		fmt.Fprintf(w.out, format+"\n", args...)
	}
}

// Warn writes a warning to stderr (non-JSON mode only).
func (w *Writer) Warn(format string, args ...any) {
	if !w.json {
		fmt.Fprintf(w.errOut, "Warning: "+format+"\n", args...)
	}
}

// Error writes to stderr.
func (w *Writer) Error(format string, args ...any) {
	fmt.Fprintf(w.errOut, "Error: "+format+"\n", args...)
}

// JSON writes an arbitrary value as indented JSON to stdout.
// Returns true if JSON mode is active (and the value was written),
// false if the caller should fall through to human output.
func (w *Writer) JSON(v any) bool {
	if !w.json {
		return false
	}
	enc := json.NewEncoder(w.out)
	enc.SetIndent("", "  ")
	enc.Encode(v)
	return true
}

// Table prints tabulated output for ls commands.
func (w *Writer) Table(headers []string, rows [][]string) {
	if w.json {
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

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
