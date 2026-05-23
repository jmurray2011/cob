package output

import (
	"fmt"
	"io"
	"sync"

	"github.com/jmurray2011/cob/internal/cob"
)

// streamRenderer prints one terminal line per asset event, with no
// in-place updates. It is the fallback for non-TTY targets (CI logs,
// pipes), and the explicit choice when --no-tui / COB_TUI=0 is set on
// an interactive terminal. Concurrent goroutines call its methods, so
// access is serialized to avoid line interleaving.
//
// AssetStart and AssetProgress are intentionally silent: a CI log
// shouldn't fill with "started" lines that immediately get superseded by
// "OK" lines, and per-byte progress would flood the log. Only the
// terminal states (OK/FAIL/SKIP) emit, one line each.
type streamRenderer struct {
	out io.Writer
	mu  sync.Mutex
}

func (s *streamRenderer) AssetsExpected(int, int64)        {}
func (s *streamRenderer) AssetStart(string, string, int64) {}
func (s *streamRenderer) AssetProgress(string, int64)      {}

func (s *streamRenderer) AssetOK(r *cob.AssetResult, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "OK %s (%s) %s %s\n",
		r.Name, FormatSize(r.Size), FormatDuration(r.DurationMs), r.Method)
}

func (s *streamRenderer) AssetFail(name, _ string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "FAIL %s %s\n", name, err)
}

func (s *streamRenderer) AssetSkipped(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "SKIP %s\n", name)
}

func (s *streamRenderer) Close() {}
