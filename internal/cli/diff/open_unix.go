//go:build unix

package diff

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openLeafNoFollow opens path for reading with O_NOFOLLOW so a symlink
// at the final path component fails with ELOOP rather than redirecting
// the open to an unintended file. Used by dir-mode diff hashing — the
// dir's contract is "compare the regular file at this basename to the
// published asset", and a local symlink (planted, or `ln -s` by
// accident) must not silently change the comparison target.
//
// Atomic relative to a TOCTOU swap: unlike Lstat-then-Open, the kernel
// performs the symlink check during open itself. The non-unix
// fall-through file (diff_open_other.go) uses Lstat for portability —
// on Windows, where neither O_NOFOLLOW nor a reliable symlink threat
// model applies the same way, the gap is acceptable.
func openLeafNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// errors.Is(err, syscall.ELOOP) would work, but the wrapped
		// PathError message ("too many levels of symbolic links") is
		// kernel-speak; rewrite for the operator who actually triggered
		// this by having a symlink where a regular file was expected.
		var pe *os.PathError
		if errors.As(err, &pe) && pe.Err == syscall.ELOOP {
			return nil, fmt.Errorf("refusing to hash symlink %s (dir-mode diff only compares regular files at the basename)", path)
		}
		return nil, err
	}
	return f, nil
}
