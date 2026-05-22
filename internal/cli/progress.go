package cli

import (
	"fmt"
	"sync"
	"time"

	"github.com/jmurray2011/cob/internal/output"
)

// progressMeter aggregates byte-count deltas from concurrent transfers into a
// single throttled status line. add is concurrency-safe and is handed to the
// cob layer as its Progress hook; finish clears the line. When the writer
// can't show progress (non-TTY, --json, --quiet) every method is a cheap
// no-op, so callers can wire it unconditionally.
type progressMeter struct {
	out   *output.Writer
	total int64 // 0 when the total isn't known up front

	mu       sync.Mutex
	done     int64
	start    time.Time
	lastShow time.Time
}

func newProgressMeter(out *output.Writer, total int64) *progressMeter {
	return &progressMeter{out: out, total: total, start: time.Now()}
}

// add records delta bytes and re-renders the line at most ~6x/second.
func (p *progressMeter) add(delta int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += delta
	if time.Since(p.lastShow) < 160*time.Millisecond {
		return
	}
	p.lastShow = time.Now()
	p.out.Progress(p.line())
}

// finish clears the progress line once the transfers are done.
func (p *progressMeter) finish() { p.out.ClearProgress() }

func (p *progressMeter) line() string {
	var rate string
	if secs := time.Since(p.start).Seconds(); secs > 0 {
		rate = "  " + output.FormatSize(int64(float64(p.done)/secs)) + "/s"
	}
	if p.total > 0 {
		return fmt.Sprintf("transferring  %s / %s  (%d%%)%s",
			output.FormatSize(p.done), output.FormatSize(p.total), p.done*100/p.total, rate)
	}
	return fmt.Sprintf("transferring  %s%s", output.FormatSize(p.done), rate)
}
