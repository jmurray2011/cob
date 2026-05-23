package cliutil

import "testing"

// FuzzSafeJoin checks the core security invariant: whenever SafeJoin accepts
// an asset name, the path it returns stays within root. Any input that would
// escape must be rejected, and no input may panic.
func FuzzSafeJoin(f *testing.F) {
	for _, s := range []string{
		"app.bin", "sub/dir/app.bin", "../escape", "..", "a/../../b",
		"/abs", "./rel", "", "x/../../../etc/passwd", "...", "a/./b",
	} {
		f.Add(s)
	}
	root := f.TempDir()
	f.Fuzz(func(t *testing.T, name string) {
		got, err := SafeJoin(root, name)
		if err != nil {
			return // rejected — the safe outcome
		}
		if escapes(root, got) {
			t.Errorf("SafeJoin(%q, %q) = %q, which escapes root", root, name, got)
		}
	})
}
