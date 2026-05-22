package cli

import (
	"fmt"

	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

// ExitError carries an intended process exit code out of a run* function.
// The command functions return it instead of calling os.Exit, so they stay
// unit-testable; main is the single place that turns it into a process exit
// status. By the time an ExitError is returned the user-facing message has
// already been written via output.Writer (stderr, plus a JSON CommandResult
// in --json mode), so main must not print it again.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// fail emits the error (stderr, and a minimal JSON CommandResult under --json)
// and returns an ExitError carrying code. It replaces the old
// `out.ErrorResult(cmd, msg); os.Exit(code)` pair.
func fail(out *output.Writer, command string, code int, format string, args ...any) error {
	out.ErrorResult(command, fmt.Sprintf(format, args...))
	return &ExitError{Code: code}
}

// codeFor maps an error to an exit code: ExitNotFound when it represents a
// missing package/version/asset, ExitError otherwise — so a network/throttle
// failure isn't misreported to CI as "not found".
func codeFor(err error) int {
	if cob.IsNotFound(err) {
		return cob.ExitNotFound
	}
	return cob.ExitError
}
