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
	isTTY := isTerminal(os.Stdout)
	return &Writer{
		out:    os.Stdout,
		errOut: os.Stderr,
		json:   jsonMode,
		isTTY:  isTTY,
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

// AssetOK prints a successful asset transfer line.
func (w *Writer) AssetOK(r *cob.AssetResult, sourceURI string) {
	if w.json {
		return
	}
	sizeStr := FormatSize(r.Size)
	if w.isTTY {
		durStr := FormatDuration(r.DurationMs)
		fmt.Fprintf(w.out, "  OK %-12s <- %-45s (%s)  %s  %s\n",
			r.Name, truncateURI(sourceURI, 45), sizeStr, durStr, r.Method)
	} else {
		durStr := FormatDuration(r.DurationMs)
		fmt.Fprintf(w.out, "OK %s (%s) %s\n", r.Name, sizeStr, durStr)
	}
}

// AssetFail prints a failed asset line.
func (w *Writer) AssetFail(name, sourceURI string, err error) {
	if w.json {
		return
	}
	if w.isTTY {
		fmt.Fprintf(w.out, "  FAIL %-12s <- %-45s %s\n", name, truncateURI(sourceURI, 45), err)
	} else {
		fmt.Fprintf(w.out, "FAIL %s %s\n", name, err)
	}
}

// AssetSkipped prints a skipped asset line.
func (w *Writer) AssetSkipped(name string) {
	if w.json {
		return
	}
	if w.isTTY {
		fmt.Fprintf(w.out, "       %-12s (skipped)\n", name)
	} else {
		fmt.Fprintf(w.out, "SKIP %s\n", name)
	}
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

func truncateURI(uri string, max int) string {
	if len(uri) <= max {
		return uri
	}
	return uri[:max-3] + "..."
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
