//go:build !unix

package diff

import (
	"fmt"
	"os"
)

// openLeafNoFollow on non-unix platforms (Windows) falls back to an
// Lstat-then-Open check. The unix companion file uses O_NOFOLLOW for an
// atomic guarantee; Windows lacks a portable equivalent in syscall, and
// its symlink semantics differ enough that the Lstat probe is a
// pragmatic best-effort. The TOCTOU window between Lstat and Open is
// theoretically exploitable by a concurrent local attacker — for the
// dir-mode diff threat model (the user wants to compare their own
// files), that gap is acceptable.
func openLeafNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to hash symlink %s (dir-mode diff only compares regular files at the basename)", path)
	}
	return os.Open(path)
}
