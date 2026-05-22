package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/output"
)

func TestProgressMeter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.NewWithWriters(&stdout, &stderr, false) // non-TTY
	m := newProgressMeter(w, 1000)

	m.add(250)
	if m.done != 250 {
		t.Errorf("done = %d, want 250", m.done)
	}
	if got := m.line(); !strings.Contains(got, "25%") {
		t.Errorf("line = %q, want a 25%% reading", got)
	}
	m.add(750)
	if m.done != 1000 {
		t.Errorf("done = %d, want 1000", m.done)
	}
	m.finish()

	// A non-TTY writer must not let progress leak onto stdout or stderr.
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("progress wrote to non-TTY output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
