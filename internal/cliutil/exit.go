package cliutil

import (
	"fmt"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// ExitError carries an intended process exit code out of a run* function.
// The command functions return it instead of calling os.Exit, so they stay
// unit-testable; main is the single place that turns it into a process exit
// status. By the time an ExitError is returned the user-facing message has
// already been written via output.Writer (stderr, plus a JSON CommandResult
// in --json mode), so main must not print it again.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Fail emits the error (stderr, and a minimal JSON CommandResult under --json)
// and returns an ExitError carrying code. Used by every run* on the
// command-level error paths so the user sees the message exactly once and
// main translates the code to a process exit status.
func Fail(out *output.Writer, command string, code int, format string, args ...any) error {
	out.ErrorResult(command, fmt.Sprintf(format, args...))
	return &ExitError{Code: code}
}

// CodeFor maps an error to an exit code: ExitNotFound when it represents a
// missing package/version/asset, ExitError otherwise — so a network/throttle
// failure isn't misreported to CI as "not found".
func CodeFor(err error) int {
	if cob.IsNotFound(err) {
		return cob.ExitNotFound
	}
	return cob.ExitError
}
